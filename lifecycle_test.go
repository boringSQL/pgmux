package pgmux

import (
	"context"
	"log/slog"
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
