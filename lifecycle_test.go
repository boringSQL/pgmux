package pgmux

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"
)

// A closed listener must end the accept loop. Before this was handled, Accept
// returned net.ErrClosed forever, the error arm logged and continued, and the
// loop span at full tilt writing one line per iteration until the process died.
func TestAcceptLoopStopsOnClosedListener(t *testing.T) {
	var logs syncBuffer
	addr := pickFreePort(t)
	proxy := NewProxyServer(addr, NewStaticRouter(nil))
	proxy.WithLogger(slog.New(slog.NewTextHandler(&logs, nil)))

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	dialUntilReady(t, addr).Close()

	// Close the listener without cancelling the context — the case Shutdown
	// will create, and the one the ctx.Done() arm does not cover.
	proxy.mu.RLock()
	listener := proxy.listener
	proxy.mu.RUnlock()
	if listener == nil {
		t.Fatal("listener not recorded on the server")
	}
	listener.Close()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil for a closed listener", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after the listener was closed")
	}

	if got := logs.String(); strings.Contains(got, "failed to accept") {
		t.Errorf("accept loop logged an error instead of returning:\n%s", got)
	}
}

// startForShutdown starts a proxy the test will stop itself, and returns the
// address plus the channel carrying Start's return value.
func startForShutdown(t *testing.T, backend *BackendConfig) (*ProxyServer, string, <-chan error) {
	t.Helper()
	addr := pickFreePort(t)
	proxy := NewProxyServer(addr, NewStaticRouter(nil).WithFallback(backend))
	proxy.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	dialUntilReady(t, addr).Close()
	t.Cleanup(func() { proxy.Shutdown(context.Background()) })
	return proxy, addr, done
}

func TestShutdownDrainsIdleServer(t *testing.T) {
	proxy, _, done := startForShutdown(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := proxy.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

// A session that outlives the deadline must not be waited on for ever, and the
// connection must actually be closed rather than merely abandoned.
func TestShutdownForceClosesAtDeadline(t *testing.T) {
	fb := newFakeBackend(t)
	proxy, addr, done := startForShutdown(t, fb.config("guest", "showcase"))

	conn := authenticated(t, addr)
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := proxy.Shutdown(ctx)
	if err == nil {
		t.Fatal("Shutdown returned nil, want the drain deadline error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Shutdown error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), "1 connection") {
		t.Errorf("Shutdown error = %q, want the count of force-closed connections", err)
	}
	if elapsed := time.Since(start); elapsed > forceCloseGrace+time.Second {
		t.Errorf("Shutdown took %v, want it bounded by the grace period", elapsed)
	}

	// The session was cut, not left running: the client must reach EOF.
	// Drain rather than reading one byte — the startup exchange leaves
	// ReadyForQuery buffered, which a single Read would consume either way.
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.Copy(io.Discard, conn); err != nil {
		t.Errorf("client connection did not reach EOF after a forced shutdown: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after Shutdown")
	}
}

func TestShutdownBeforeStartIsSafe(t *testing.T) {
	proxy := NewProxyServer(pickFreePort(t), NewStaticRouter(nil))
	if err := proxy.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown before Start: %v", err)
	}
}

func TestShutdownIsIdempotent(t *testing.T) {
	proxy, _, _ := startForShutdown(t, nil)
	for i := range 2 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := proxy.Shutdown(ctx); err != nil {
			t.Fatalf("Shutdown %d: %v", i+1, err)
		}
		cancel()
	}
}

// A connection accepted in the window between Accept returning and the listener
// closing must be dropped, not tracked: wg.Add after wg.Wait panics.
func TestShutdownRejectsLateConnections(t *testing.T) {
	proxy, _, _ := startForShutdown(t, nil)
	if err := proxy.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if tracked := proxy.trackConn(&net.TCPConn{}); tracked {
		t.Error("trackConn accepted a connection after Shutdown")
	}
}

// Start on a server that has already been shut down must return, not serve an
// accept-and-reject loop for ever.
func TestStartAfterShutdownReturns(t *testing.T) {
	proxy := NewProxyServer(pickFreePort(t), NewStaticRouter(nil))
	proxy.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err := proxy.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start is still running after Shutdown")
	}
}

// A forced Shutdown must cut the backend connection too: closing only the
// client side leaves a session parked in a backend write.
func TestForceCloseCutsBackendConns(t *testing.T) {
	proxy, _, _ := startForShutdown(t, nil)

	clientProxy, clientEnd := net.Pipe()
	defer clientEnd.Close()
	if !proxy.trackConn(clientProxy) {
		t.Fatal("trackConn rejected a connection before Shutdown")
	}
	defer proxy.untrackConn(clientProxy)

	backendProxy, backendEnd := net.Pipe()
	defer backendEnd.Close()
	if !proxy.trackBackendConn(backendProxy) {
		t.Fatal("trackBackendConn rejected a connection before Shutdown")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := proxy.Shutdown(ctx); err == nil {
		t.Fatal("Shutdown returned nil, want the drain deadline error")
	}

	for name, end := range map[string]net.Conn{"client": clientEnd, "backend": backendEnd} {
		end.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := end.Read(make([]byte, 1)); err != io.EOF {
			t.Errorf("%s conn: read = %v, want EOF after forced Shutdown", name, err)
		}
	}
}
