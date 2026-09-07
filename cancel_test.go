package pgmux

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// cancelBackend is a fake PostgreSQL that answers startups with a known
// BackendKeyData and records any CancelRequest it receives.
type cancelBackend struct {
	ln              net.Listener
	pid             uint32
	secret          uint32
	scram           bool
	strayAfterReady bool
	cancels         chan *pgproto3.CancelRequest
}

// doSASL runs a minimal SCRAM-shaped exchange. The point is not the crypto but
// the message order: on this path AuthenticationOk arrives from an inner loop,
// and BackendKeyData follows it.
func (cb *cancelBackend) doSASL(conn net.Conn, be *pgproto3.Backend) bool {
	req, _ := (&pgproto3.AuthenticationSASL{AuthMechanisms: []string{"SCRAM-SHA-256"}}).Encode(nil)
	conn.Write(req)

	be.SetAuthType(pgproto3.AuthTypeSASL)
	if _, err := be.Receive(); err != nil {
		return false
	}
	cont, _ := (&pgproto3.AuthenticationSASLContinue{Data: []byte("r=x,s=y,i=4096")}).Encode(nil)
	conn.Write(cont)

	be.SetAuthType(pgproto3.AuthTypeSASLContinue)
	if _, err := be.Receive(); err != nil {
		return false
	}
	fin, _ := (&pgproto3.AuthenticationSASLFinal{Data: []byte("v=z")}).Encode(nil)
	conn.Write(fin)
	return true
}

func newCancelBackend(t *testing.T) *cancelBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	cb := &cancelBackend{
		ln:      ln,
		pid:     4242,
		secret:  0xDEADBEEF,
		cancels: make(chan *pgproto3.CancelRequest, 8),
	}
	go cb.serve()
	t.Cleanup(func() { ln.Close() })
	return cb
}

func (cb *cancelBackend) serve() {
	for {
		conn, err := cb.ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			be := pgproto3.NewBackend(pgproto3.NewChunkReader(conn), conn)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := be.ReceiveStartupMessage()
			if err != nil {
				return
			}
			conn.SetReadDeadline(time.Time{})

			switch m := msg.(type) {
			case *pgproto3.CancelRequest:
				select {
				case cb.cancels <- m:
				default:
				}
			case *pgproto3.StartupMessage:
				if cb.scram && !cb.doSASL(conn, be) {
					return
				}
				ok, _ := (&pgproto3.AuthenticationOk{}).Encode(nil)
				conn.Write(ok)
				kd, _ := (&pgproto3.BackendKeyData{ProcessID: cb.pid, SecretKey: cb.secret}).Encode(nil)
				conn.Write(kd)
				rfq, _ := (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil)
				conn.Write(rfq)
				if cb.strayAfterReady {
					stray, _ := (&pgproto3.BackendKeyData{ProcessID: cb.pid, SecretKey: cb.secret}).Encode(nil)
					conn.Write(stray)
				}
				io.Copy(io.Discard, conn)
			}
		}(conn)
	}
}

func (cb *cancelBackend) config() *BackendConfig {
	host, portStr, _ := net.SplitHostPort(cb.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return &BackendConfig{Host: host, Port: port, User: "guest", Database: "showcase"}
}

func (cb *cancelBackend) awaitCancel(t *testing.T) *pgproto3.CancelRequest {
	t.Helper()
	select {
	case c := <-cb.cancels:
		return c
	case <-time.After(3 * time.Second):
		t.Fatal("backend never received a cancel request")
		return nil
	}
}

// startSession opens a session and returns the BackendKeyData the client saw.
func startSession(t *testing.T, addr string) (net.Conn, *pgproto3.BackendKeyData) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write startup: %v", err)
	}

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("startup exchange: %v", err)
		}
		if kd, ok := msg.(*pgproto3.BackendKeyData); ok {
			return conn, kd
		}
	}
}

func sendCancel(t *testing.T, addr string, pid, secret uint32) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial for cancel: %v", err)
	}
	defer conn.Close()
	buf, _ := (&pgproto3.CancelRequest{ProcessID: pid, SecretKey: secret}).Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write cancel: %v", err)
	}
}

func cancelProxy(t *testing.T, cb *cancelBackend) string {
	t.Helper()
	addr, _ := cancelProxyServer(t, cb, nil)
	return addr
}

func cancelProxyServer(t *testing.T, cb *cancelBackend, configure func(*ProxyServer)) (string, *ProxyServer) {
	t.Helper()
	addr := pickFreePort(t)
	proxy := NewProxyServer(addr, NewStaticRouter(nil).WithFallback(cb.config()))
	proxy.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if configure != nil {
		configure(proxy)
	}
	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	t.Cleanup(func() { proxy.Shutdown(context.Background()); <-done })
	dialUntilReady(t, addr).Close()
	return addr, proxy
}

func registrySize(ps *ProxyServer) int {
	ps.cancelMu.Lock()
	defer ps.cancelMu.Unlock()
	return len(ps.cancelKeys)
}

// The client's key must be the proxy's, and the cancel that reaches the
// backend must carry the backend's own.
func TestCancelRequestIsForwardedWithBackendKey(t *testing.T) {
	cb := newCancelBackend(t)
	addr := cancelProxy(t, cb)

	_, clientKey := startSession(t, addr)
	if clientKey.ProcessID == cb.pid && clientKey.SecretKey == cb.secret {
		t.Fatal("the backend's own key was relayed to the client")
	}

	sendCancel(t, addr, clientKey.ProcessID, clientKey.SecretKey)

	got := cb.awaitCancel(t)
	if got.ProcessID != cb.pid || got.SecretKey != cb.secret {
		t.Errorf("backend received pid=%d secret=%d, want %d/%d",
			got.ProcessID, got.SecretKey, cb.pid, cb.secret)
	}
}

// An unknown key must be indistinguishable from a known one: any difference
// turns the endpoint into an oracle for probing live cancel keys.
func TestUnknownCancelKeyIsClosedSilently(t *testing.T) {
	cb := newCancelBackend(t)
	addr := cancelProxy(t, cb)
	startSession(t, addr)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()
	buf, _ := (&pgproto3.CancelRequest{ProcessID: 1, SecretKey: 2}).Encode(nil)
	conn.Write(buf)

	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(make([]byte, 1))
	if err == nil || n > 0 {
		t.Errorf("unknown cancel key drew a response (n=%d, err=%v)", n, err)
	}

	select {
	case c := <-cb.cancels:
		t.Errorf("unknown key reached the backend: %+v", c)
	case <-time.After(300 * time.Millisecond):
	}
}

// Keys must not outlive their session, or the registry grows without bound and
// a stale key cancels whatever now holds that backend PID.
func TestCancelKeyForgottenWhenSessionEnds(t *testing.T) {
	cb := newCancelBackend(t)
	addr, proxy := cancelProxyServer(t, cb, nil)

	conn, _ := startSession(t, addr)
	if got := registrySize(proxy); got != 1 {
		t.Fatalf("registry holds %d keys during the session, want 1", got)
	}
	conn.Close()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if registrySize(proxy) == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("registry still holds %d keys after the session ended", registrySize(proxy))
}

// Distinct sessions must get distinct keys.
func TestCancelKeysAreUnique(t *testing.T) {
	cb := newCancelBackend(t)
	addr := cancelProxy(t, cb)

	seen := make(map[cancelKey]bool)
	for range 5 {
		_, kd := startSession(t, addr)
		key := cancelKey{pid: kd.ProcessID, secret: kd.SecretKey}
		if seen[key] {
			t.Fatalf("duplicate cancel key issued: %+v", key)
		}
		seen[key] = true
	}
}

// The registry is per-session state and must be safe under concurrency.
func TestCancelRegistryIsConcurrent(t *testing.T) {
	cb := newCancelBackend(t)
	addr := cancelProxy(t, cb)

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, kd, err := trySession(addr)
			if err != nil {
				errs <- err
				return
			}
			defer conn.Close()
			if err := trySendCancel(addr, kd.ProcessID, kd.SecretKey); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent session: %v", err)
	}
}

// A plaintext cancel must be refused when TLS is required, like any other
// startup-phase message.
func TestCancelRequiresTLSWhenRequired(t *testing.T) {
	cb := newCancelBackend(t)
	certFile, keyFile := writeTestCert(t)
	addr, _ := cancelProxyServer(t, cb, func(p *ProxyServer) {
		p.WithTLS(&TLSConfig{Enabled: true, Required: true, CertFile: certFile, KeyFile: keyFile})
	})

	// A real session over TLS, so the key sent below is genuinely valid: an
	// unknown key would be dropped for the wrong reason and prove nothing.
	clientKey := tlsSessionKey(t, addr)
	sendCancel(t, addr, clientKey.ProcessID, clientKey.SecretKey)

	select {
	case c := <-cb.cancels:
		t.Errorf("plaintext cancel with a valid key reached the backend: %+v", c)
	case <-time.After(500 * time.Millisecond):
	}
}

// scramSession completes the client half of the SCRAM-shaped exchange and
// returns the BackendKeyData the client ends up with.
func scramSession(t *testing.T, addr string) (net.Conn, *pgproto3.BackendKeyData) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	conn.Write(buf)

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("SASL exchange: %v", err)
		}
		switch msg.(type) {
		case *pgproto3.AuthenticationSASL:
			resp, _ := (&pgproto3.SASLInitialResponse{
				AuthMechanism: "SCRAM-SHA-256", Data: []byte("n,,n=,r=x"),
			}).Encode(nil)
			conn.Write(resp)
		case *pgproto3.AuthenticationSASLContinue:
			resp, _ := (&pgproto3.SASLResponse{Data: []byte("c=biws,r=x,p=q")}).Encode(nil)
			conn.Write(resp)
		case *pgproto3.BackendKeyData:
			return conn, msg.(*pgproto3.BackendKeyData)
		}
	}
}

// SCRAM is the deployed configuration, and its AuthenticationOk arrives from an
// inner loop: a cancel key must still be issued and still work.
func TestCancelWorksOverSCRAM(t *testing.T) {
	cb := newCancelBackend(t)
	cb.scram = true
	addr := cancelProxy(t, cb)

	_, clientKey := scramSession(t, addr)
	if clientKey.ProcessID == cb.pid && clientKey.SecretKey == cb.secret {
		t.Fatal("the backend's own key was relayed to the client over SCRAM")
	}
	if clientKey.ProcessID == 0 && clientKey.SecretKey == 0 {
		t.Fatal("no cancel key was issued over SCRAM")
	}

	sendCancel(t, addr, clientKey.ProcessID, clientKey.SecretKey)

	got := cb.awaitCancel(t)
	if got.ProcessID != cb.pid || got.SecretKey != cb.secret {
		t.Errorf("backend received pid=%d secret=%d, want %d/%d",
			got.ProcessID, got.SecretKey, cb.pid, cb.secret)
	}
}

// trySession and trySendCancel are the goroutine-safe forms of the helpers
// above: t.Fatalf outside the test goroutine skips the failure and hangs.
func trySession(addr string) (net.Conn, *pgproto3.BackendKeyData, error) {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return nil, nil, err
	}
	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		conn.Close()
		return nil, nil, err
	}

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	defer conn.SetReadDeadline(time.Time{})
	for {
		msg, err := fe.Receive()
		if err != nil {
			conn.Close()
			return nil, nil, err
		}
		if kd, ok := msg.(*pgproto3.BackendKeyData); ok {
			return conn, kd, nil
		}
	}
}

func trySendCancel(addr string, pid, secret uint32) error {
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	buf, _ := (&pgproto3.CancelRequest{ProcessID: pid, SecretKey: secret}).Encode(nil)
	_, err = conn.Write(buf)
	return err
}

// tlsSessionKey opens a session over TLS and returns the issued cancel key.
func tlsSessionKey(t *testing.T, addr string) *pgproto3.BackendKeyData {
	t.Helper()
	raw, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { raw.Close() })

	sslReq, _ := (&pgproto3.SSLRequest{}).Encode(nil)
	raw.Write(sslReq)
	raw.SetDeadline(time.Now().Add(5 * time.Second))
	resp := make([]byte, 1)
	if _, err := io.ReadFull(raw, resp); err != nil || resp[0] != 'S' {
		t.Fatalf("SSL negotiation: resp=%q err=%v", resp, err)
	}
	tlsConn := tls.Client(raw, &tls.Config{InsecureSkipVerify: true})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("handshake: %v", err)
	}

	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	tlsConn.Write(buf)

	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(tlsConn), tlsConn)
	for {
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("startup over TLS: %v", err)
		}
		if kd, ok := msg.(*pgproto3.BackendKeyData); ok {
			raw.SetDeadline(time.Time{})
			return kd
		}
	}
}

// A BackendKeyData arriving after authentication is not part of the protocol,
// and forwarding one would hand back the real PID the substitution withheld.
func TestStrayBackendKeyDataIsNotRelayed(t *testing.T) {
	cb := newCancelBackend(t)
	cb.strayAfterReady = true
	addr := cancelProxy(t, cb)

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

	// One reader for the whole exchange: a second Frontend would leave the
	// stray message sitting in the first one's buffer and prove nothing.
	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	conn.SetReadDeadline(time.Now().Add(time.Second))

	var keys []*pgproto3.BackendKeyData
	for {
		msg, err := fe.Receive()
		if err != nil {
			break
		}
		if kd, ok := msg.(*pgproto3.BackendKeyData); ok {
			keys = append(keys, kd)
		}
	}

	if len(keys) != 1 {
		t.Fatalf("client saw %d BackendKeyData messages, want exactly 1", len(keys))
	}
	if keys[0].ProcessID == cb.pid || keys[0].SecretKey == cb.secret {
		t.Error("client holds the backend's real key")
	}
}
