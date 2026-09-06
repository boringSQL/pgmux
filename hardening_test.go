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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// authenticated opens a client connection through the proxy and completes the
// startup exchange against the fake backend.
func authenticated(t *testing.T, addr string) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn).Receive(); err != nil {
		t.Fatalf("startup exchange: %v", err)
	}
	conn.SetReadDeadline(time.Time{})
	return conn
}

// ClientIdleTimeout must close the connection AND release its slot. Before the
// relay goroutines signalled unconditionally, an idle timeout returned silently
// and left the slot held for the lifetime of the process.
func TestIdleTimeoutReleasesConnectionSlot(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxConnections: 1, ClientIdleTimeout: 300 * time.Millisecond})
	})

	c1 := authenticated(t, addr)
	defer c1.Close()

	// Idle past the timeout without closing, the way a NAT-dropped client does.
	time.Sleep(1500 * time.Millisecond)

	// The only slot must be free again.
	c2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial 2: %v", err)
	}
	defer c2.Close()
	assertNotRejected(t, c2)
}

// The same guarantee for an abrupt client disconnect rather than a clean close.
func TestClientResetReleasesConnectionSlot(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxConnections: 1})
	})

	c1 := authenticated(t, addr)
	// SO_LINGER 0 makes Close send RST instead of FIN.
	if tcp, ok := c1.(*net.TCPConn); ok {
		tcp.SetLinger(0)
	}
	c1.Close()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c2, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial 2: %v", err)
		}
		c2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(c2), c2)
		msg, rerr := fe.Receive()
		rejected := false
		if rerr == nil {
			if e, ok := msg.(*pgproto3.ErrorResponse); ok && e.Code == "53300" {
				rejected = true
			}
		}
		c2.Close()
		if !rejected {
			return // slot was released
		}
		if time.Now().After(deadline) {
			t.Fatal("connection slot never released after a client reset")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A backend that vanishes mid-session must also release the slot.
func TestBackendDisconnectReleasesConnectionSlot(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxConnections: 1})
	})

	c1 := authenticated(t, addr)
	defer c1.Close()

	// Simulate backend death: the listener goes down and the established
	// connection gets a FIN. Closing the listener alone leaves accepted
	// connections untouched.
	fb.ln.Close()
	fb.closeAll()

	deadline := time.Now().Add(5 * time.Second)
	for {
		c2, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial 2: %v", err)
		}
		c2.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(c2), c2)
		msg, rerr := fe.Receive()
		rejected := rerr == nil
		if rerr == nil {
			e, ok := msg.(*pgproto3.ErrorResponse)
			rejected = ok && e.Code == "53300"
		}
		c2.Close()
		if !rejected {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("connection slot never released after the backend disconnected")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// A GSSENCRequest must be declined and the following startup packet honoured.
// libpq sends it whenever the client holds Kerberos credentials.
func TestGSSEncRequestIsDeclined(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// GSSENCRequest: int32 length (8) + int32 code 80877104.
	if _, err := conn.Write([]byte{0x00, 0x00, 0x00, 0x08, 0x04, 0xd2, 0x16, 0x30}); err != nil {
		t.Fatalf("write GSSENCRequest: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp := make([]byte, 1)
	if _, err := io.ReadFull(conn, resp); err != nil {
		t.Fatalf("no response to GSSENCRequest (connection dropped): %v", err)
	}
	if resp[0] != 'N' {
		t.Fatalf("expected 'N' declining GSS encryption, got %q", resp[0])
	}

	// The connection must still be usable for the real StartupMessage.
	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	conn.Write(buf)

	msg, err := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn).Receive()
	if err != nil {
		t.Fatalf("startup after declined GSS: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk, got %T", msg)
	}
	if got := fb.awaitStartup(t).Parameters["database"]; got != "showcase" {
		t.Errorf("database not rewritten after GSS path: got %q", got)
	}
}

// End-to-end TLS: real handshake, real StartupMessage over TLS, and the
// startup deadline must not survive into the established session.
func TestTLSEndToEndAndDeadlineCleared(t *testing.T) {
	certFile, keyFile := writeTestCert(t)
	fb := newFakeBackend(t)

	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithTLS(&TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile})
	})

	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()

	sslReq, _ := (&pgproto3.SSLRequest{}).Encode(nil)
	raw.Write(sslReq)

	raw.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp := make([]byte, 1)
	if _, err := io.ReadFull(raw, resp); err != nil {
		t.Fatalf("SSL negotiation: %v", err)
	}
	if resp[0] != 'S' {
		t.Fatalf("expected 'S', got %q", resp[0])
	}
	raw.SetReadDeadline(time.Time{})

	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("TLS handshake: %v", err)
	}

	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	tlsConn.Write(buf)

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(tlsConn), tlsConn)
	tlsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	msg, err := fe.Receive()
	if err != nil {
		t.Fatalf("startup over TLS: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk over TLS, got %T", msg)
	}
	if got := fb.awaitStartup(t).Parameters["database"]; got != "showcase" {
		t.Errorf("database not rewritten over TLS: got %q", got)
	}

	// The startup deadline must have been cleared: an established session that
	// stays quiet longer than startupTimeout must not be torn down.
	tlsConn.SetReadDeadline(time.Now().Add(startupTimeout + 3*time.Second))
	done := make(chan error, 1)
	go func() {
		_, err := fe.Receive()
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil && !strings.Contains(err.Error(), "i/o timeout") {
			t.Errorf("session died while idle: %v", err)
		}
	case <-time.After(startupTimeout + 2*time.Second):
		// Still open past the startup timeout: correct.
	}
}

// Start must fail on an unreadable or malformed keypair, not defer the failure
// to the first client's handshake.
func TestStartValidatesCertificatesUpFront(t *testing.T) {
	cases := []struct {
		name    string
		cfg     *TLSConfig
		wantErr string
	}{
		{"no certificates", &TLSConfig{Enabled: true}, "no certificates provided"},
		{"missing file", &TLSConfig{Enabled: true, CertFile: "/nonexistent/a.crt", KeyFile: "/nonexistent/a.key"}, "load TLS keypair"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := NewProxyServer("127.0.0.1:0", NewStaticRouter(nil)).WithTLS(tc.cfg)
			p.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
			err := p.Start(t.Context())
			if err == nil {
				t.Fatal("expected Start to fail")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("expected error containing %q, got %q", tc.wantErr, err)
			}
		})
	}
}

// Over-capacity rejections must not be counted as accepted connections.
func TestOverCapacityNotCountedAsAccepted(t *testing.T) {
	fb := newFakeBackend(t)
	var buf syncBuffer
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxConnections: 1})
		p.WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	})
	time.Sleep(100 * time.Millisecond)
	buf.Reset()

	c1 := authenticated(t, addr)
	defer c1.Close()

	// Three rejections while the single slot is occupied.
	for range 3 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		assertRejectedOverCapacity(t, c)
		c.Close()
	}
	time.Sleep(200 * time.Millisecond)

	if got := strings.Count(buf.String(), `msg="connection accepted"`); got != 1 {
		t.Errorf("expected exactly 1 'connection accepted' line for 1 accepted connection, got %d:\n%s",
			got, buf.String())
	}
}

// A rejection must not stall the accept loop when the rejected client never
// reads; the next legitimate client must still be served promptly.
func TestRejectionDoesNotStallAcceptLoop(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxConnections: 1})
	})

	c1 := authenticated(t, addr)
	defer c1.Close()

	// Several non-reading clients that will be rejected.
	for range 5 {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		defer c.Close()
	}

	// The accept loop must still respond quickly.
	start := time.Now()
	probe, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("probe dial: %v", err)
	}
	defer probe.Close()
	assertRejectedOverCapacity(t, probe)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("accept loop stalled: probe took %s", elapsed)
	}
}

func writeTestCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certFile, keyFile
}

// A backend ErrorResponse during authentication must reach the client without
// PostgreSQL's internal fields. A pg_hba rejection carries the proxy host's
// address and the PG source file that raised it.
func TestBackendAuthErrorIsSanitized(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	// A backend that rejects with a fully populated error, as PostgreSQL does.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				be := pgproto3.NewBackend(pgproto3.NewChunkReader(conn), conn)
				if _, err := be.ReceiveStartupMessage(); err != nil {
					return
				}
				resp := &pgproto3.ErrorResponse{
					Severity: "FATAL",
					Code:     "28000",
					Message:  `no pg_hba.conf entry for host "10.0.1.7", user "guest", database "showcase", no encryption`,
					Detail:   "internal detail leak",
					Hint:     "internal hint leak",
					Where:    "internal where leak",
					File:     "auth.c",
					Line:     396,
					Routine:  "ClientAuthentication",
				}
				buf, _ := resp.Encode(nil)
				conn.Write(buf)
			}(conn)
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(&BackendConfig{
			Host: host, Port: port, User: "guest", Database: "showcase",
		})
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	conn.Write(buf)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msg, err := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn).Receive()
	if err != nil {
		t.Fatalf("expected an ErrorResponse: %v", err)
	}
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected *pgproto3.ErrorResponse, got %T", msg)
	}

	// The reason must survive so a legitimate user can act on it.
	if errResp.Code != "28000" {
		t.Errorf("expected SQLSTATE 28000 to survive, got %q", errResp.Code)
	}
	// The internals must not.
	for name, got := range map[string]string{
		"Detail": errResp.Detail, "Hint": errResp.Hint,
		"Where": errResp.Where, "File": errResp.File, "Routine": errResp.Routine,
	} {
		if got != "" {
			t.Errorf("backend %s leaked to client: %q", name, got)
		}
	}
	if errResp.Line != 0 {
		t.Errorf("backend source Line leaked to client: %d", errResp.Line)
	}
}

// A slow backend must not cause the session to be torn down: if any startup or
// handshake read deadline survives into the established session, the client is
// disconnected mid-query. This reproduces "SELECT pg_sleep(14)" without a real
// PostgreSQL.
func TestSlowBackendDoesNotTripStartupDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("sleeps past startupTimeout")
	}
	delay := startupTimeout + 4*time.Second

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				be := pgproto3.NewBackend(pgproto3.NewChunkReader(conn), conn)
				if _, err := be.ReceiveStartupMessage(); err != nil {
					return
				}
				ok, _ := (&pgproto3.AuthenticationOk{}).Encode(nil)
				conn.Write(ok)
				rfq, _ := (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil)
				conn.Write(rfq)

				// Client sends a query; answer only after a long delay, during
				// which neither side writes anything.
				if _, err := be.Receive(); err != nil {
					return
				}
				time.Sleep(delay)
				cc, _ := (&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")}).Encode(nil)
				conn.Write(cc)
				rfq2, _ := (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil)
				conn.Write(rfq2)
			}(conn)
		}
	}()

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	port, _ := strconv.Atoi(portStr)

	for _, mode := range []string{"plain", "tls"} {
		t.Run(mode, func(t *testing.T) {
			var certFile, keyFile string
			if mode == "tls" {
				certFile, keyFile = writeTestCert(t)
			}
			addr := startProxy(t, func(p *ProxyServer) {
				p.router = NewStaticRouter(nil).WithFallback(&BackendConfig{
					Host: host, Port: port, User: "guest", Database: "showcase",
				})
				if mode == "tls" {
					p.WithTLS(&TLSConfig{Enabled: true, CertFile: certFile, KeyFile: keyFile})
				}
			})

			var conn net.Conn
			raw, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatalf("dial: %v", err)
			}
			defer raw.Close()
			conn = raw

			if mode == "tls" {
				sslReq, _ := (&pgproto3.SSLRequest{}).Encode(nil)
				raw.Write(sslReq)
				raw.SetReadDeadline(time.Now().Add(3 * time.Second))
				resp := make([]byte, 1)
				if _, err := io.ReadFull(raw, resp); err != nil || resp[0] != 'S' {
					t.Fatalf("SSL negotiation failed: %v (%q)", err, resp)
				}
				raw.SetReadDeadline(time.Time{})
				tc := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
				if err := tc.Handshake(); err != nil {
					t.Fatalf("handshake: %v", err)
				}
				conn = tc
			}

			sm := &pgproto3.StartupMessage{
				ProtocolVersion: pgproto3.ProtocolVersionNumber,
				Parameters:      map[string]string{"user": "alice", "database": "alice"},
			}
			buf, _ := sm.Encode(nil)
			conn.Write(buf)

			fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if _, err := fe.Receive(); err != nil { // AuthenticationOk
				t.Fatalf("auth: %v", err)
			}
			if _, err := fe.Receive(); err != nil { // ReadyForQuery
				t.Fatalf("ready for query: %v", err)
			}

			// Issue a query the backend will answer only after `delay`.
			q, _ := (&pgproto3.Query{String: "SELECT pg_sleep(14)"}).Encode(nil)
			if _, err := conn.Write(q); err != nil {
				t.Fatalf("send query: %v", err)
			}

			conn.SetReadDeadline(time.Now().Add(delay + 10*time.Second))
			msg, err := fe.Receive()
			if err != nil {
				t.Fatalf("session died while the backend was working (%s mode): %v", mode, err)
			}
			if _, ok := msg.(*pgproto3.CommandComplete); !ok {
				t.Fatalf("expected CommandComplete, got %T", msg)
			}
		})
	}
}
