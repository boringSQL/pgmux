package pgmux

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// requireTLSProxy starts a proxy that refuses plaintext, in front of fb.
func requireTLSProxy(t *testing.T, fb *fakeBackend) string {
	t.Helper()
	certFile, keyFile := writeTestCert(t)
	return startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithTLS(&TLSConfig{Enabled: true, Required: true, CertFile: certFile, KeyFile: keyFile})
	})
}

// assertSSLRequired reads the proxy's reply and asserts it is the FATAL that
// refuses plaintext — not an AuthenticationOk, and not a dropped connection.
func assertSSLRequired(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	msg, err := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn).Receive()
	if err != nil {
		t.Fatalf("no response to a plaintext startup: %v", err)
	}
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("plaintext startup got %T, want an ErrorResponse", msg)
	}
	if errResp.Code != "28000" || errResp.Message != msgSSLRequired {
		t.Fatalf("got %s %q, want 28000 %q", errResp.Code, errResp.Message, msgSSLRequired)
	}
}

// The direct path: a client that never sends an SSLRequest at all.
func TestRequiredRejectsPlaintextStartup(t *testing.T) {
	fb := newFakeBackend(t)
	addr := requireTLSProxy(t, fb)

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
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	assertSSLRequired(t, conn)

	select {
	case sm := <-fb.startups:
		t.Fatalf("plaintext connection reached the backend: %+v", sm.Parameters)
	case <-time.After(300 * time.Millisecond):
	}
}

// The path a guard in the SSLRequest branch would miss: libpq sends
// GSSENCRequest first when gssencmode is prefer, which is the default.
// psql "gssencmode=prefer sslmode=disable" arrives here.
func TestRequiredRejectsPlaintextAfterDeclinedGSS(t *testing.T) {
	fb := newFakeBackend(t)
	addr := requireTLSProxy(t, fb)

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
		t.Fatalf("no response to GSSENCRequest: %v", err)
	}
	if resp[0] != 'N' {
		t.Fatalf("expected 'N' declining GSS, got %q", resp[0])
	}

	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write startup: %v", err)
	}
	assertSSLRequired(t, conn)

	select {
	case sm := <-fb.startups:
		t.Fatalf("plaintext connection reached the backend via GSS: %+v", sm.Parameters)
	case <-time.After(300 * time.Millisecond):
	}
}

// Requiring TLS must not break the clients that do use it.
func TestRequiredAdmitsTLSConnections(t *testing.T) {
	fb := newFakeBackend(t)
	addr := requireTLSProxy(t, fb)

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

	tlsConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	msg, err := pgproto3.NewFrontend(pgproto3.NewChunkReader(tlsConn), tlsConn).Receive()
	if err != nil {
		t.Fatalf("startup over TLS: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("got %T over TLS, want AuthenticationOk", msg)
	}
	if got := fb.awaitStartup(t).Parameters["database"]; got != "showcase" {
		t.Errorf("database = %q, want the rewrite to still apply", got)
	}
}

// Required without Enabled protects nothing, so it must not start.
func TestRequiredWithoutEnabledFailsToStart(t *testing.T) {
	proxy := NewProxyServer("127.0.0.1:0", NewStaticRouter(nil))
	proxy.WithTLS(&TLSConfig{Required: true})

	err := proxy.Start(context.Background())
	if err == nil {
		t.Fatal("Start accepted Required without Enabled")
	}
	if err.Error() != "TLS required but not enabled" {
		t.Errorf("error = %q", err)
	}
}
