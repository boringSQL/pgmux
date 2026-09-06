package pgmux

import (
	"crypto/tls"
	"testing"
)

// Config used to be returned verbatim, so setting MinVersion meant giving up
// CertFile/KeyFile and loading the keypair yourself.
func TestResolveServerTLSCombinesConfigAndCertFiles(t *testing.T) {
	certFile, keyFile := writeTestCert(t)

	resolved, err := resolveServerTLS(&TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
		Config:   &tls.Config{MinVersion: tls.VersionTLS13},
	}, nil)
	if err != nil {
		t.Fatalf("resolveServerTLS: %v", err)
	}
	if resolved.MinVersion != tls.VersionTLS13 {
		t.Errorf("MinVersion = %#x, want TLS 1.3 from Config", resolved.MinVersion)
	}
	// Files are served through GetCertificate so renewals are picked up.
	if resolved.GetCertificate == nil {
		t.Error("no GetCertificate for a keypair loaded from CertFile")
	}
	if _, err := resolved.GetCertificate(&tls.ClientHelloInfo{}); err != nil {
		t.Errorf("GetCertificate: %v", err)
	}
}

// A Config that already carries certificates must not have them replaced from
// disk, and must keep working with no CertFile/KeyFile set at all.
func TestResolveServerTLSKeepsConfigCertificates(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatalf("load keypair: %v", err)
	}

	resolved, err := resolveServerTLS(&TLSConfig{
		Enabled: true,
		Config:  &tls.Config{Certificates: []tls.Certificate{cert}},
	}, nil)
	if err != nil {
		t.Fatalf("resolveServerTLS: %v", err)
	}
	if len(resolved.Certificates) != 1 {
		t.Fatalf("got %d certificates, want the one from Config", len(resolved.Certificates))
	}
}

// The caller's config is theirs: resolving must not write certificates back
// into it, or a config shared between two servers picks up one server's cert.
func TestResolveServerTLSDoesNotMutateCallerConfig(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	caller := &tls.Config{MinVersion: tls.VersionTLS12}

	if _, err := resolveServerTLS(&TLSConfig{
		Enabled: true, CertFile: certFile, KeyFile: keyFile, Config: caller,
	}, nil); err != nil {
		t.Fatalf("resolveServerTLS: %v", err)
	}
	if len(caller.Certificates) != 0 {
		t.Error("resolveServerTLS wrote certificates into the caller's config")
	}
}

// A Config carrying no certificates is a startup error, not a server that
// fails every handshake at runtime.
func TestResolveServerTLSRejectsConfigWithoutCertificates(t *testing.T) {
	_, err := resolveServerTLS(&TLSConfig{
		Enabled: true,
		Config:  &tls.Config{MinVersion: tls.VersionTLS13},
	}, nil)
	if err == nil {
		t.Fatal("resolveServerTLS accepted a Config with no certificates")
	}
	if err.Error() != "TLS enabled but no certificates provided" {
		t.Errorf("error = %q", err)
	}
}
