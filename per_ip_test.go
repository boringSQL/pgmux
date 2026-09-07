package pgmux

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

func perIPProxy(t *testing.T, limits *Limits) (*ProxyServer, string, *syncBuffer) {
	t.Helper()
	addr := pickFreePort(t)
	logs := &syncBuffer{}
	proxy := NewProxyServer(addr, NewStaticRouter(nil)).WithLimits(limits)
	proxy.WithLogger(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	t.Cleanup(func() { proxy.Shutdown(context.Background()); <-done })
	// Return only once the readiness probe has been counted and its slot
	// returned, so a test's baseline stats are not racing it.
	dialUntilReady(t, addr).Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if st := proxy.Stats(); st.Accepted >= 1 && st.Current == 0 {
			return proxy, addr, logs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("readiness probe never settled: %+v", proxy.Stats())
	return nil, "", nil
}

// One host must not be able to take every slot. All test connections share
// 127.0.0.1, which is the case the limit exists for.
func TestMaxConnectionsPerIPRejectsExcess(t *testing.T) {
	proxy, addr, _ := perIPProxy(t, &Limits{MaxConnections: 10, MaxConnectionsPerIP: 2})
	baseline := proxy.Stats()

	for i := range 2 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("conn %d dial: %v", i, err)
		}
		defer conn.Close()
	}
	waitForCurrent(t, proxy, 2)

	third, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("third dial: %v", err)
	}
	defer third.Close()
	assertRejectedOverCapacity(t, third)

	got := proxy.Stats()
	if got.Rejected != baseline.Rejected+1 {
		t.Errorf("Rejected = %d, want %d", got.Rejected, baseline.Rejected+1)
	}
	if got.Accepted != baseline.Accepted+2 {
		t.Errorf("Accepted = %d, want %d — the refused connection must not count", got.Accepted, baseline.Accepted+2)
	}
}

// A closed session must hand its slot back.
func TestMaxConnectionsPerIPReleasesSlots(t *testing.T) {
	proxy, addr, _ := perIPProxy(t, &Limits{MaxConnections: 10, MaxConnectionsPerIP: 1})

	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	waitForCurrent(t, proxy, 1)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer second.Close()
	assertRejectedOverCapacity(t, second)

	first.Close()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		third, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		rejected := isRejected(third)
		third.Close()
		if !rejected {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("slot was never released after the session closed")
}

// The default must not change behaviour for existing callers.
func TestMaxConnectionsPerIPDefaultsToUnlimited(t *testing.T) {
	_, addr, _ := perIPProxy(t, &Limits{MaxConnections: 10})

	for i := range 5 {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatalf("conn %d dial: %v", i, err)
		}
		defer conn.Close()
		assertNotRejected(t, conn)
	}
}

// Distinct sources are accounted separately. Exercised directly because every
// connection in this suite arrives from 127.0.0.1.
func TestReserveIPIsPerAddress(t *testing.T) {
	proxy := NewProxyServer("127.0.0.1:0", NewStaticRouter(nil)).
		WithLimits(&Limits{MaxConnectionsPerIP: 1})

	if !proxy.reserveIP("10.0.0.1") {
		t.Fatal("first reservation for 10.0.0.1 refused")
	}
	if proxy.reserveIP("10.0.0.1") {
		t.Error("second reservation for 10.0.0.1 accepted over the limit")
	}
	if !proxy.reserveIP("10.0.0.2") {
		t.Error("a different address was refused while 10.0.0.1 was at its limit")
	}

	proxy.releaseIP("10.0.0.1")
	if !proxy.reserveIP("10.0.0.1") {
		t.Error("slot not returned after release")
	}
}

// The map is keyed by attacker-supplied addresses, so it must not retain them.
func TestReleaseIPCleansUpTheMap(t *testing.T) {
	proxy := NewProxyServer("127.0.0.1:0", NewStaticRouter(nil)).
		WithLimits(&Limits{MaxConnectionsPerIP: 2})

	proxy.reserveIP("10.0.0.1")
	proxy.reserveIP("10.0.0.1")
	proxy.releaseIP("10.0.0.1")
	proxy.releaseIP("10.0.0.1")

	proxy.ipMu.Lock()
	defer proxy.ipMu.Unlock()
	if len(proxy.perIP) != 0 {
		t.Errorf("perIP retained %d entries after release", len(proxy.perIP))
	}
}

// Rejections must not be loggable at default level: past the cap an
// unauthenticated client would otherwise set the log volume.
func TestRejectionIsNotLoggedAtDefaultLevel(t *testing.T) {
	addr := pickFreePort(t)
	logs := &syncBuffer{}
	proxy := NewProxyServer(addr, NewStaticRouter(nil)).
		WithLimits(&Limits{MaxConnections: 1, MaxConnectionsPerIP: 1})
	proxy.WithLogger(slog.New(slog.NewTextHandler(logs, nil))) // default level: Info

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	t.Cleanup(func() { proxy.Shutdown(context.Background()); <-done })

	first := dialUntilReady(t, addr)
	defer first.Close()
	waitForCurrent(t, proxy, 1)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer second.Close()
	assertRejectedOverCapacity(t, second)

	if strings.Contains(logs.String(), "rejecting connection") {
		t.Errorf("rejection logged at default level:\n%s", logs.String())
	}
	got := proxy.Stats()
	if got.Rejected != 1 || got.RejectedPerIP != 1 {
		t.Errorf("Stats() = %+v, want rejected 1 and per-IP 1", got)
	}
}

// isRejected reports whether the proxy answered immediately, which at this
// point in the exchange can only be the FATAL refusing the connection: an
// accepted connection waits for a startup message and says nothing.
func isRejected(conn net.Conn) bool {
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	defer conn.SetReadDeadline(time.Time{})
	_, err := io.ReadFull(conn, make([]byte, 1))
	return err == nil
}

// waitForCurrent blocks until the proxy reports want live sessions, so tests
// do not race the accept loop or a handler that has yet to release its slot.
func waitForCurrent(t *testing.T, proxy *ProxyServer, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if proxy.Stats().Current == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("Stats().Current = %d, want %d", proxy.Stats().Current, want)
}

// stubAddrConn is a net.Conn that reports a chosen remote address.
type stubAddrConn struct {
	net.Conn
	addr net.Addr
}

func (c stubAddrConn) RemoteAddr() net.Addr { return c.addr }

type namedAddr struct{ s string }

func (a namedAddr) Network() string { return "pipe" }
func (a namedAddr) String() string  { return a.s }

func TestRemoteIPKeying(t *testing.T) {
	key := func(addr net.Addr) string { return remoteIP(stubAddrConn{addr: addr}) }

	t.Run("ipv4-mapped matches ipv4", func(t *testing.T) {
		// A dual-stack listener must not count one client under two keys.
		four := key(&net.TCPAddr{IP: net.IPv4(1, 2, 3, 4), Port: 5000})
		mapped := key(&net.TCPAddr{IP: net.ParseIP("::ffff:1.2.3.4"), Port: 5001})
		if four != mapped {
			t.Errorf("IPv4 %q and IPv4-mapped %q produced different keys", four, mapped)
		}
		if four != "1.2.3.4" {
			t.Errorf("key = %q, want 1.2.3.4", four)
		}
	})

	t.Run("ipv6 shares a key across a /64", func(t *testing.T) {
		// The whole point: a /64 is one end site, and keying on /128 would
		// hand an attacker 2^64 keys.
		a := key(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2::1"), Port: 5000})
		b := key(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2:dead:beef:cafe:1"), Port: 5001})
		if a != b {
			t.Errorf("addresses in one /64 produced different keys: %q and %q", a, b)
		}
	})

	t.Run("ipv6 separates distinct /64s", func(t *testing.T) {
		a := key(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:2::1"), Port: 5000})
		b := key(&net.TCPAddr{IP: net.ParseIP("2001:db8:1:3::1"), Port: 5001})
		if a == b {
			t.Errorf("addresses in different /64s shared key %q", a)
		}
	})

	t.Run("distinct ipv4 addresses differ", func(t *testing.T) {
		if key(&net.TCPAddr{IP: net.IPv4(1, 2, 3, 4)}) == key(&net.TCPAddr{IP: net.IPv4(1, 2, 3, 5)}) {
			t.Error("distinct IPv4 addresses shared a key")
		}
	})

	t.Run("non-TCP address falls back", func(t *testing.T) {
		// net.Pipe and friends have no host:port; they must not panic, even
		// though they all collapse to one key.
		if got := key(namedAddr{s: "pipe"}); got != "pipe" {
			t.Errorf("key = %q, want the address verbatim", got)
		}
		if got := key(namedAddr{s: "10.0.0.1:5432"}); got != "10.0.0.1" {
			t.Errorf("key = %q, want the host part", got)
		}
	})
}

// The global-cap rejection is the other half of the Warn->Debug demotion, and
// with a per-IP limit set it is never reached: the per-IP check fires first.
func TestGlobalCapRejectionIsNotLoggedAtDefaultLevel(t *testing.T) {
	addr := pickFreePort(t)
	logs := &syncBuffer{}
	proxy := NewProxyServer(addr, NewStaticRouter(nil)).
		WithLimits(&Limits{MaxConnections: 1, MaxConnectionsPerIP: 0})
	proxy.WithLogger(slog.New(slog.NewTextHandler(logs, nil))) // default level: Info

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	t.Cleanup(func() { proxy.Shutdown(context.Background()); <-done })

	first := dialUntilReady(t, addr)
	defer first.Close()
	waitForCurrent(t, proxy, 1)

	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer second.Close()
	assertRejectedOverCapacity(t, second)

	if strings.Contains(logs.String(), "rejecting connection") {
		t.Errorf("global-cap rejection logged at default level:\n%s", logs.String())
	}
	got := proxy.Stats()
	if got.Rejected != 1 {
		t.Errorf("Rejected = %d, want 1", got.Rejected)
	}
	if got.RejectedPerIP != 0 {
		t.Errorf("RejectedPerIP = %d, want 0 for a global-cap refusal", got.RejectedPerIP)
	}
}
