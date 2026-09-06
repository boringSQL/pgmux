package pgmux

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// writeCertAs writes a keypair whose CommonName identifies which generation it
// is, so a test can tell one from the next across a reload.
func writeCertAs(t *testing.T, certFile, keyFile, commonName string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// servedCommonName completes a TLS handshake against the proxy and reports the
// CommonName of the certificate it presented.
func servedCommonName(t *testing.T, addr string) string {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()

	sslReq, _ := (&pgproto3.SSLRequest{}).Encode(nil)
	if _, err := raw.Write(sslReq); err != nil {
		t.Fatalf("write SSLRequest: %v", err)
	}
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	resp := make([]byte, 1)
	if _, err := io.ReadFull(raw, resp); err != nil {
		t.Fatalf("SSL negotiation: %v", err)
	}
	if resp[0] != 'S' {
		t.Fatalf("expected 'S', got %q", resp[0])
	}

	// Deadline kept across the handshake: a regression that stalls it should
	// fail here, not hang until the package timeout.
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}
	raw.SetDeadline(time.Time{})
	defer tlsConn.Close()
	return tlsConn.ConnectionState().PeerCertificates[0].Subject.CommonName
}

func reloadingProxy(t *testing.T) (addr, certFile, keyFile string, logs *syncBuffer) {
	t.Helper()
	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	writeCertAs(t, certFile, keyFile, "aaaaa")

	logs = &syncBuffer{}
	addr = startProxy(t, func(p *ProxyServer) {
		p.WithLogger(slog.New(slog.NewTextHandler(logs, nil)))
		p.WithTLS(&TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile})
	})
	return addr, certFile, keyFile, logs
}

// A renewal on disk must reach the next handshake without a restart. The
// replacement CommonName is the same length as the original and the mtimes are
// set explicitly, so this exercises the mtime path rather than passing by a
// size coincidence on a coarse-grained filesystem.
func TestCertificateReloadedOnChange(t *testing.T) {
	addr, certFile, keyFile, _ := reloadingProxy(t)

	if got := servedCommonName(t, addr); got != "aaaaa" {
		t.Fatalf("served CommonName = %q, want %q", got, "aaaaa")
	}

	writeCertAs(t, certFile, keyFile, "bbbbb")
	touch(t, certFile, 2*time.Second)
	touch(t, keyFile, 2*time.Second)

	if got := servedCommonName(t, addr); got != "bbbbb" {
		t.Errorf("served CommonName = %q after renewal, want %q", got, "bbbbb")
	}
}

// A half-written or corrupt file must not take the endpoint down.
func TestCorruptCertificateKeepsServingPrevious(t *testing.T) {
	addr, certFile, _, logs := reloadingProxy(t)

	if got := servedCommonName(t, addr); got != "aaaaa" {
		t.Fatalf("served CommonName = %q, want %q", got, "aaaaa")
	}

	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}

	if got := servedCommonName(t, addr); got != "aaaaa" {
		t.Errorf("served CommonName = %q after a corrupt write, want the previous %q", got, "aaaaa")
	}
	if !strings.Contains(logs.String(), "TLS certificate reload failed") {
		t.Error("a failed reload was not logged")
	}
}

// Unchanged files must not be reloaded, or every handshake pays for a keypair
// parse and the log fills with reload lines.
func TestUnchangedCertificateIsNotReloaded(t *testing.T) {
	addr, _, _, logs := reloadingProxy(t)

	for range 3 {
		servedCommonName(t, addr)
	}
	if strings.Contains(logs.String(), "TLS certificate reloaded") {
		t.Error("an unchanged certificate was reloaded")
	}
}

// A bad path must still fail at startup rather than on the first handshake.
func TestCertReloaderRejectsMissingFiles(t *testing.T) {
	dir := t.TempDir()
	_, err := newCertReloader(filepath.Join(dir, "nope.crt"), filepath.Join(dir, "nope.key"), nil)
	if err == nil {
		t.Fatal("newCertReloader accepted a missing certificate")
	}
}

// touch moves a file's mtime forward, so a change is detectable regardless of
// filesystem timestamp granularity.
func touch(t *testing.T, path string, ahead time.Duration) {
	t.Helper()
	when := time.Now().Add(ahead)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

func countLines(haystack, needle string) int {
	return strings.Count(haystack, needle)
}

// The realistic certbot failure is transient: the cert symlink is relinked
// before the key, so one load fails and the next must succeed.
func TestCertificateRecoversAfterCorruption(t *testing.T) {
	addr, certFile, keyFile, logs := reloadingProxy(t)

	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	if got := servedCommonName(t, addr); got != "aaaaa" {
		t.Fatalf("served CommonName = %q during corruption, want the previous %q", got, "aaaaa")
	}

	writeCertAs(t, certFile, keyFile, "ccccc")
	touch(t, certFile, 2*time.Second)
	touch(t, keyFile, 2*time.Second)

	if got := servedCommonName(t, addr); got != "ccccc" {
		t.Errorf("served CommonName = %q after recovery, want %q", got, "ccccc")
	}
	if n := countLines(logs.String(), "TLS certificate reloaded"); n != 1 {
		t.Errorf("logged %d reloads, want 1", n)
	}
}

// A file that stays broken must be reported once, not once per handshake: an
// unauthenticated client would otherwise control log volume by reconnecting.
func TestPersistentlyCorruptCertificateLogsOnce(t *testing.T) {
	addr, certFile, _, logs := reloadingProxy(t)

	if err := os.WriteFile(certFile, []byte("-----BEGIN CERTIFICATE-----\nnope\n"), 0o600); err != nil {
		t.Fatalf("corrupt cert: %v", err)
	}
	for range 5 {
		if got := servedCommonName(t, addr); got != "aaaaa" {
			t.Fatalf("served CommonName = %q, want the previous %q", got, "aaaaa")
		}
	}

	if n := countLines(logs.String(), "TLS certificate reload failed"); n != 1 {
		t.Errorf("logged %d failures over 5 handshakes, want 1", n)
	}
}

// Concurrent handshakes across a renewal must collapse to a single reload.
func TestConcurrentHandshakesReloadOnce(t *testing.T) {
	addr, certFile, keyFile, logs := reloadingProxy(t)
	servedCommonName(t, addr)

	writeCertAs(t, certFile, keyFile, "ddddd")
	touch(t, certFile, 2*time.Second)
	touch(t, keyFile, 2*time.Second)

	var wg sync.WaitGroup
	names := make([]string, 8)
	for i := range names {
		wg.Add(1)
		go func() {
			defer wg.Done()
			names[i] = servedCommonName(t, addr)
		}()
	}
	wg.Wait()

	for i, got := range names {
		if got != "ddddd" {
			t.Errorf("handshake %d served %q, want %q", i, got, "ddddd")
		}
	}
	if n := countLines(logs.String(), "TLS certificate reloaded"); n != 1 {
		t.Errorf("logged %d reloads for one renewal, want 1", n)
	}
}
