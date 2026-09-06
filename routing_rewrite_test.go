package pgmux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// fakeBackend is a minimal PostgreSQL server: it accepts one connection,
// records the StartupMessage it was sent, answers AuthenticationOk and then
// ReadyForQuery. Enough to exercise the proxy's rewrite path end to end.
type fakeBackend struct {
	ln       net.Listener
	startups chan *pgproto3.StartupMessage
	rawFirst chan []byte
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
}

func newFakeBackend(t *testing.T) *fakeBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("fake backend listen: %v", err)
	}
	fb := &fakeBackend{
		ln:       ln,
		startups: make(chan *pgproto3.StartupMessage, 8),
		rawFirst: make(chan []byte, 8),
		conns:    make(map[net.Conn]struct{}),
	}
	go fb.serve()
	t.Cleanup(func() { ln.Close() })
	return fb
}

func (fb *fakeBackend) serve() {
	for {
		conn, err := fb.ln.Accept()
		if err != nil {
			return
		}
		go func(conn net.Conn) {
			defer conn.Close()
			fb.mu.Lock()
			fb.conns[conn] = struct{}{}
			fb.mu.Unlock()
			defer func() {
				fb.mu.Lock()
				delete(fb.conns, conn)
				fb.mu.Unlock()
			}()
			// Capture the raw startup bytes as well as the parsed message, so a
			// test can assert byte-for-byte equality across configurations.
			rec := &recordingReader{r: conn}
			be := pgproto3.NewBackend(pgproto3.NewChunkReader(rec), conn)
			conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			msg, err := be.ReceiveStartupMessage()
			if err != nil {
				return
			}
			// The deadline guards the startup exchange only; an established
			// session may idle arbitrarily long.
			conn.SetReadDeadline(time.Time{})
			sm, ok := msg.(*pgproto3.StartupMessage)
			if !ok {
				return
			}
			select {
			case fb.startups <- sm:
			default:
			}
			select {
			case fb.rawFirst <- rec.bytes():
			default:
			}
			ok0, _ := (&pgproto3.AuthenticationOk{}).Encode(nil)
			conn.Write(ok0)
			rfq, _ := (&pgproto3.ReadyForQuery{TxStatus: 'I'}).Encode(nil)
			conn.Write(rfq)
			io.Copy(io.Discard, conn)
		}(conn)
	}
}

func (fb *fakeBackend) config(user, database string) *BackendConfig {
	host, portStr, _ := net.SplitHostPort(fb.ln.Addr().String())
	port, _ := strconv.Atoi(portStr)
	return &BackendConfig{Host: host, Port: port, User: user, Database: database}
}

// closeAll closes every accepted connection, simulating backend process
// death: the established connections get a FIN, which is what the proxy must
// notice. Closing the listener alone does not touch accepted connections.
func (fb *fakeBackend) closeAll() {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	for conn := range fb.conns {
		conn.Close()
	}
}

func (fb *fakeBackend) awaitStartup(t *testing.T) *pgproto3.StartupMessage {
	t.Helper()
	select {
	case sm := <-fb.startups:
		return sm
	case <-time.After(3 * time.Second):
		t.Fatal("backend never received a startup message")
		return nil
	}
}

type recordingReader struct {
	r   io.Reader
	buf []byte
}

func (rr *recordingReader) Read(p []byte) (int, error) {
	n, err := rr.r.Read(p)
	rr.buf = append(rr.buf, p[:n]...)
	return n, err
}

func (rr *recordingReader) bytes() []byte { return rr.buf }

// startProxy runs a proxy on a free port and returns its address.
func startProxy(t *testing.T, configure func(*ProxyServer)) string {
	t.Helper()
	addr := pickFreePort(t)
	router := NewStaticRouter(nil)
	proxy := NewProxyServer(addr, router)
	// Keep test output quiet; debug lines are asserted elsewhere.
	proxy.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))
	configure(proxy)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { proxy.Start(ctx); close(done) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
		}
	})
	dialUntilReady(t, addr).Close()
	return addr
}

// clientConnect sends a StartupMessage with the given parameters and reads
// until AuthenticationOk or an ErrorResponse.
func clientConnect(t *testing.T, addr string, params map[string]string) (pgproto3.BackendMessage, error) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	sm := &pgproto3.StartupMessage{ProtocolVersion: pgproto3.ProtocolVersionNumber, Parameters: params}
	buf, _ := sm.Encode(nil)
	if _, err := conn.Write(buf); err != nil {
		t.Fatalf("write startup: %v", err)
	}

	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	return fe.Receive()
}

// Acceptance 1 & 2: any username, with or without a database, lands on the
// configured backend user and database.
func TestStartupRewriteUserAndDatabase(t *testing.T) {
	fb := newFakeBackend(t)
	backend := fb.config("guest", "showcase")

	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(backend)
	})

	cases := []struct {
		name   string
		params map[string]string
	}{
		// libpq with neither -U nor -d: both default to the OS username.
		{"no flags", map[string]string{"user": "alice", "database": "alice"}},
		{"-U guest", map[string]string{"user": "guest", "database": "guest"}},
		{"-U anything -d anything", map[string]string{"user": "anything", "database": "anything"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			msg, err := clientConnect(t, addr, tc.params)
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
				t.Fatalf("expected AuthenticationOk, got %T (%+v)", msg, msg)
			}
			sm := fb.awaitStartup(t)
			if got := sm.Parameters["user"]; got != "guest" {
				t.Errorf("user: expected %q, got %q", "guest", got)
			}
			if got := sm.Parameters["database"]; got != "showcase" {
				t.Errorf("database: expected %q, got %q", "showcase", got)
			}
		})
	}
}

// Acceptance 4: Database == "" leaves the client's database untouched, and the
// StartupMessage forwarded to the backend is unchanged from before the feature.
//
// Note on "byte-for-byte": pgproto3 encodes StartupMessage parameters by
// ranging over a Go map, so the wire order is randomised per call and byte
// equality is not a property pgmux has (or had before this change). The
// meaningful invariant, and the one SQL Labs depends on, is that the parameter
// set is identical — same keys, same values, nothing added or dropped.
func TestEmptyDatabaseLeavesStartupUnchanged(t *testing.T) {
	params := map[string]string{"user": "app_user", "database": "clientdb", "application_name": "psql"}

	forward := func(t *testing.T, database string) (map[string]string, int) {
		fb := newFakeBackend(t)
		addr := startProxy(t, func(p *ProxyServer) {
			p.router = NewStaticRouter(map[string]*BackendConfig{
				"app_user": fb.config("postgres", database),
			})
		})
		if _, err := clientConnect(t, addr, params); err != nil {
			t.Fatalf("connect: %v", err)
		}
		sm := fb.awaitStartup(t)
		var size int
		select {
		case b := <-fb.rawFirst:
			size = len(b)
		case <-time.After(2 * time.Second):
			t.Fatal("no raw startup bytes")
		}
		return sm.Parameters, size
	}

	// Database unset: the client's own database must survive untouched, and no
	// parameter may be added or dropped.
	got, gotSize := forward(t, "")
	want := map[string]string{"user": "postgres", "database": "clientdb", "application_name": "psql"}
	if !maps.Equal(got, want) {
		t.Errorf("Database==\"\" changed the parameter set:\n got: %+v\nwant: %+v", got, want)
	}

	// A second identical run must forward the same set and the same number of
	// wire bytes; only the ordering within the message may differ.
	again, againSize := forward(t, "")
	if !maps.Equal(got, again) {
		t.Errorf("forwarded parameters not stable across runs:\n first: %+v\nsecond: %+v", got, again)
	}
	if gotSize != againSize {
		t.Errorf("StartupMessage size not stable: %d then %d bytes", gotSize, againSize)
	}

	// And with Database set, only that one key changes.
	set, _ := forward(t, "showcase")
	wantSet := map[string]string{"user": "postgres", "database": "showcase", "application_name": "psql"}
	if !maps.Equal(set, wantSet) {
		t.Errorf("Database set changed more than the database key:\n got: %+v\nwant: %+v", set, wantSet)
	}
}

// Acceptance 5: fallback is opt-in.
func TestStaticRouterFallback(t *testing.T) {
	ctx := context.Background()
	mapped := &BackendConfig{Host: "mapped.example.com", Port: 5432, User: "mapped"}
	fallback := &BackendConfig{Host: "fallback.example.com", Port: 5432, User: "guest"}

	t.Run("no fallback still returns ErrUserNotFound", func(t *testing.T) {
		r := NewStaticRouter(map[string]*BackendConfig{"known": mapped})
		if _, err := r.Route(ctx, "unmapped"); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})

	t.Run("fallback catches unmapped users", func(t *testing.T) {
		r := NewStaticRouter(map[string]*BackendConfig{"known": mapped}).WithFallback(fallback)
		got, err := r.Route(ctx, "unmapped")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != fallback {
			t.Errorf("expected fallback config, got %+v", got)
		}
	})

	t.Run("explicit mappings still win over fallback", func(t *testing.T) {
		r := NewStaticRouter(map[string]*BackendConfig{"known": mapped}).WithFallback(fallback)
		got, err := r.Route(ctx, "known")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got != mapped {
			t.Errorf("expected mapped config, got %+v", got)
		}
	})

	t.Run("nil fallback restores strict behaviour", func(t *testing.T) {
		r := NewStaticRouter(nil).WithFallback(fallback).WithFallback(nil)
		if _, err := r.Route(ctx, "anyone"); !errors.Is(err, ErrUserNotFound) {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})
}

// Acceptance 6: the hook sees the client address and the rewritten values, and
// its mutations reach the backend.
func TestStartupRewriteHook(t *testing.T) {
	fb := newFakeBackend(t)

	type seen struct {
		addr     net.Addr
		user     string
		database string
	}
	seenCh := make(chan seen, 1)

	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithStartupRewrite(func(clientAddr net.Addr, params map[string]string) {
			seenCh <- seen{clientAddr, params["user"], params["database"]}
			params["application_name"] = "pgmux/" + addrString(clientAddr)
		})
	})

	if _, err := clientConnect(t, addr, map[string]string{"user": "alice", "database": "alice"}); err != nil {
		t.Fatalf("connect: %v", err)
	}

	var got seen
	select {
	case got = <-seenCh:
	case <-time.After(3 * time.Second):
		t.Fatal("hook never called")
	}

	if got.addr == nil {
		t.Error("hook received a nil client address")
	}
	if host, _, err := net.SplitHostPort(addrString(got.addr)); err != nil || host != "127.0.0.1" {
		t.Errorf("hook client address = %q, want a 127.0.0.1 address", addrString(got.addr))
	}
	// The hook must observe the already-rewritten values, not the client's.
	if got.user != "guest" {
		t.Errorf("hook saw user %q, want the rewritten %q", got.user, "guest")
	}
	if got.database != "showcase" {
		t.Errorf("hook saw database %q, want the rewritten %q", got.database, "showcase")
	}

	sm := fb.awaitStartup(t)
	if want := "pgmux/" + addrString(got.addr); sm.Parameters["application_name"] != want {
		t.Errorf("hook mutation did not reach backend: got %q, want %q",
			sm.Parameters["application_name"], want)
	}
}

// Acceptance 6b: a nil hook is a no-op.
func TestNilStartupRewriteIsNoOp(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
	})
	if _, err := clientConnect(t, addr, map[string]string{"user": "alice", "database": "alice"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	sm := fb.awaitStartup(t)
	if _, ok := sm.Parameters["application_name"]; ok {
		t.Errorf("nil hook should not add parameters, got %+v", sm.Parameters)
	}
}

// Acceptance 7: nothing sent to the client names a host, port or Go error.
func TestClientErrorsLeakNoInternals(t *testing.T) {
	secretHost := "internal-db.private.example.com"
	secretPort := 15432

	t.Run("unroutable user", func(t *testing.T) {
		addr := startProxy(t, func(p *ProxyServer) {
			p.router = NewStaticRouter(map[string]*BackendConfig{})
		})
		msg, err := clientConnect(t, addr, map[string]string{"user": "attacker-name", "database": "x"})
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		assertNoLeak(t, msg, "attacker-name", secretHost, strconv.Itoa(secretPort))
	})

	t.Run("backend unreachable", func(t *testing.T) {
		// A port with nothing listening: the dial error would normally carry
		// "dial tcp 127.0.0.1:NNNN: connect: connection refused".
		dead := pickFreePort(t)
		host, portStr, _ := net.SplitHostPort(dead)
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

		// Three dial attempts with progressive backoff take a few seconds.
		conn.SetReadDeadline(time.Now().Add(20 * time.Second))
		fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
		msg, err := fe.Receive()
		if err != nil {
			t.Fatalf("expected an ErrorResponse, got: %v", err)
		}
		assertNoLeak(t, msg, "alice", host, portStr, "dial", "refused", "connect:")
	})
}

func assertNoLeak(t *testing.T, msg pgproto3.BackendMessage, forbidden ...string) {
	t.Helper()
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected *pgproto3.ErrorResponse, got %T", msg)
	}
	if errResp.Severity != "FATAL" {
		t.Errorf("expected FATAL severity, got %q", errResp.Severity)
	}
	// Every field the client can read, not just Message.
	surface := strings.Join([]string{
		errResp.Message, errResp.Detail, errResp.Hint, errResp.Where,
		errResp.InternalQuery, errResp.File, errResp.Routine,
	}, "\x00")
	for _, f := range forbidden {
		if f == "" {
			continue
		}
		if strings.Contains(strings.ToLower(surface), strings.ToLower(f)) {
			t.Errorf("client-visible error leaks %q: %q", f, surface)
		}
	}
}

// Acceptance 8: malformed input kills one connection, not the process.
func TestMalformedStartupIsolated(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLimits(&Limits{MaxMessageSize: 4096})
	})

	junk := [][]byte{
		{0x00}, // truncated length
		{0x00, 0x00, 0x00, 0x08, 0xde, 0xad, 0xbe},                                            // truncated body
		{0xff, 0xff, 0xff, 0xff},                                                              // absurd length
		{0x00, 0x00, 0x40, 0x00, 0x00, 0x03, 0x00, 0x00},                                      // oversized vs MaxMessageSize
		append([]byte{0x00, 0x00, 0x00, 0x20, 0x00, 0x03, 0x00, 0x00}, []byte("user\x00")...), // unterminated params
	}
	for i, b := range junk {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("junk %d dial: %v", i, err)
		}
		conn.Write(b)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		io.Copy(io.Discard, conn) // expect EOF/timeout, never a crash
		conn.Close()
	}

	// The process survives and the next well-formed client is served.
	msg, err := clientConnect(t, addr, map[string]string{"user": "alice", "database": "alice"})
	if err != nil {
		t.Fatalf("proxy did not survive malformed input: %v", err)
	}
	if _, ok := msg.(*pgproto3.AuthenticationOk); !ok {
		t.Fatalf("expected AuthenticationOk after junk, got %T", msg)
	}
}

// Acceptance 9: a silent client is dropped before auth rather than holding its slot.
func TestSilentClientIsDropped(t *testing.T) {
	if testing.Short() {
		t.Skip("waits on the startup timeout")
	}
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil)
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	// Send nothing at all. The startup deadline must close the connection.
	conn.SetReadDeadline(time.Now().Add(startupTimeout + 5*time.Second))
	start := time.Now()
	_, err = conn.Read(make([]byte, 1))
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected the connection to be closed, got data")
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("connection still open after %s; startup timeout did not fire", elapsed)
	}
	if elapsed > startupTimeout+3*time.Second {
		t.Errorf("connection closed after %s, expected around %s", elapsed, startupTimeout)
	}
}

// Acceptance 11: repeated connect/disconnect leaks neither goroutines nor fds.
func TestNoLeakOverManyConnections(t *testing.T) {
	if testing.Short() {
		t.Skip("runs 1000 connections")
	}
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
	})

	// Warm up so one-time allocations are not counted as a leak.
	for range 10 {
		cycleOnce(t, addr)
	}
	settle()
	before := runtime.NumGoroutine()
	fdBefore := openFDs(t)

	for range 1000 {
		cycleOnce(t, addr)
	}

	// Goroutines unwind asynchronously after the client closes, so poll for the
	// count to come back down rather than sampling once. A real per-connection
	// leak over 1000 connections never converges; a slow teardown does.
	// The allowance covers the fake backend's own in-flight handlers.
	const allowance = 50
	deadline := time.Now().Add(30 * time.Second)
	var after int
	for {
		settle()
		after = runtime.NumGoroutine()
		if after <= before+allowance || time.Now().After(deadline) {
			break
		}
	}
	if after > before+allowance {
		t.Errorf("goroutine leak: %d before, %d after 1000 connections (allowance %d)",
			before, after, allowance)
	}

	// File descriptors: a leaked conn per cycle would show up as ~1000 extra.
	if fdBefore > 0 {
		if fdAfter := openFDs(t); fdAfter > fdBefore+allowance {
			t.Errorf("file descriptor leak: %d before, %d after 1000 connections (allowance %d)",
				fdBefore, fdAfter, allowance)
		}
	}
}

// openFDs counts this process's open descriptors. Returns 0 where /dev/fd is
// not available, and the caller skips the assertion.
func openFDs(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

func cycleOnce(t *testing.T, addr string) {
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
	conn.Write(buf)
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	fe := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	fe.Receive()
	conn.Close()
}

func settle() {
	for range 5 {
		runtime.GC()
		time.Sleep(50 * time.Millisecond)
	}
}

// A panic inside the rewrite hook must not take the process down (item 5).
func TestPanicInHookDoesNotKillProcess(t *testing.T) {
	fb := newFakeBackend(t)
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithStartupRewrite(func(_ net.Addr, params map[string]string) {
			if params["user"] == "guest" && params["database"] == "showcase" {
				panic("boom")
			}
		})
	})

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	sm := &pgproto3.StartupMessage{
		ProtocolVersion: pgproto3.ProtocolVersionNumber,
		Parameters:      map[string]string{"user": "alice", "database": "alice"},
	}
	buf, _ := sm.Encode(nil)
	conn.Write(buf)
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	io.Copy(io.Discard, conn)
	conn.Close()

	// The listener is still serving.
	probe, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("proxy died after a panicking hook: %v", err)
	}
	probe.Close()
}

// Item 5: at default level exactly one line is emitted per accepted
// connection, it carries the client address, and no client-supplied startup
// parameter appears.
func TestConnectionLoggingContract(t *testing.T) {
	fb := newFakeBackend(t)

	var buf syncBuffer
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	})

	// startProxy dials once to confirm readiness; discard that record so the
	// count below reflects only the connections this test makes.
	time.Sleep(100 * time.Millisecond)
	buf.Reset()

	const n = 5
	for i := range n {
		clientConnect(t, addr, map[string]string{
			"user":             "visitor" + strconv.Itoa(i),
			"database":         "visitor" + strconv.Itoa(i),
			"application_name": "secret-tool",
		})
	}
	time.Sleep(300 * time.Millisecond)

	out := buf.String()
	lines := strings.Split(strings.TrimSpace(out), "\n")

	accepted := 0
	for _, l := range lines {
		if strings.Contains(l, `msg="connection accepted"`) {
			accepted++
			if !strings.Contains(l, "client=127.0.0.1:") {
				t.Errorf("accepted line missing client address: %s", l)
			}
			if !strings.HasPrefix(l, "time=") {
				t.Errorf("accepted line missing timestamp: %s", l)
			}
		}
	}
	if accepted != n {
		t.Errorf("expected exactly %d 'connection accepted' lines, got %d:\n%s", n, accepted, out)
	}

	// One INFO record per connection, so the count is unambiguous.
	if len(lines) != n {
		t.Errorf("expected %d lines at default level, got %d:\n%s", n, len(lines), out)
	}

	// No client-supplied parameter value may appear at default level.
	for _, forbidden := range []string{"secret-tool", "visitor0", "visitor4"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("default-level log leaks client-supplied %q:\n%s", forbidden, out)
		}
	}
}

// Debug level does carry the detail, so operators can still get it.
func TestDebugLevelIncludesStartupParameters(t *testing.T) {
	fb := newFakeBackend(t)

	var buf syncBuffer
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	})
	clientConnect(t, addr, map[string]string{"user": "alice", "database": "alice", "application_name": "psql"})
	time.Sleep(300 * time.Millisecond)

	if out := buf.String(); !strings.Contains(out, "startup message received") || !strings.Contains(out, "alice") {
		t.Errorf("debug level should carry startup parameters, got:\n%s", out)
	}
}

// A username carrying newlines must not be able to forge a log record.
func TestLogInjectionViaUsername(t *testing.T) {
	fb := newFakeBackend(t)

	var buf syncBuffer
	addr := startProxy(t, func(p *ProxyServer) {
		p.router = NewStaticRouter(nil).WithFallback(fb.config("guest", "showcase"))
		p.WithLogger(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	})

	evil := "evil\ntime=2020-01-01T00:00:00Z level=INFO msg=\"connection accepted\" client=forged"
	clientConnect(t, addr, map[string]string{"user": evil, "database": "x"})
	time.Sleep(300 * time.Millisecond)

	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.HasPrefix(l, "time=2020-01-01") {
			t.Errorf("username forged a log record: %s", l)
		}
	}
}

// syncBuffer is a bytes.Buffer safe for concurrent writes from handler goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Reset()
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
