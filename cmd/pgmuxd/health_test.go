package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/boringsql/pgmux"
)

// testHealth returns a health server whose proxy is actually listening, since
// /healthz reports "starting" until it is.
func testHealth(t *testing.T, cfg *Config) *health {
	t.Helper()
	if cfg == nil {
		// Port 1: nothing listens there, so probes fail.
		cfg = &Config{BackendHost: "127.0.0.1", BackendPort: 1, MaxConnections: 10}
	}
	proxy := pgmux.NewProxyServer("127.0.0.1:0", pgmux.NewStaticRouter(nil)).
		WithLimits(&pgmux.Limits{MaxConnections: cfg.MaxConnections})
	proxy.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))

	done := make(chan error, 1)
	go func() { done <- proxy.Start(context.Background()) }()
	t.Cleanup(func() { proxy.Shutdown(context.Background()); <-done })

	deadline := time.Now().Add(3 * time.Second)
	for proxy.Addr() == nil && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	return newHealth(cfg, proxy, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func get(t *testing.T, h *health, remoteAddr string) (int, healthResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	req.RemoteAddr = remoteAddr
	rec := httptest.NewRecorder()
	h.handle(rec, req)

	var body healthResponse
	json.NewDecoder(rec.Body).Decode(&body)
	return rec.Code, body
}

// Before the first probe completes there is nothing to report, and claiming
// health would have Consul advertise an endpoint that may not work.
func TestHealthReportsStartingBeforeFirstProbe(t *testing.T) {
	h := testHealth(t, nil)

	code, body := get(t, h, "127.0.0.1:1234")
	if code != http.StatusServiceUnavailable || body.Status != "starting" {
		t.Errorf("status %d %q before the first probe, want 503 starting", code, body.Status)
	}
}

// A dead backend must not deregister the endpoint on the first blip: existing
// sessions are fine, and flapping out of Consul is worse than a slow database.
func TestHealthDegradesBeforeFailing(t *testing.T) {
	h := testHealth(t, nil)

	for i := 1; i < unhealthyAfter; i++ {
		h.checkLiveness(context.Background())
		code, body := get(t, h, "127.0.0.1:1234")
		if code != http.StatusOK {
			t.Fatalf("probe %d: status %d, want 200 below the failure threshold", i, code)
		}
		if body.Status != "degraded" || body.Backend.Reachable {
			t.Errorf("probe %d: status %q backend %+v", i, body.Status, body.Backend)
		}
		if body.Backend.Error == "" {
			t.Errorf("probe %d: no error reported for an unreachable backend", i)
		}
	}

	h.checkLiveness(context.Background())
	code, body := get(t, h, "127.0.0.1:1234")
	if code != http.StatusServiceUnavailable || body.Status != "unhealthy" {
		t.Errorf("status %d %q after %d failures, want 503 unhealthy", code, body.Status, unhealthyAfter)
	}
}

// Serving a health request must not probe the backend: otherwise a caller who
// hangs up mid-request counts as a backend failure, and a loop against
// /healthz turns into a loop of backend connections.
func TestHealthRequestDoesNotProbe(t *testing.T) {
	h := testHealth(t, nil)
	h.checkLiveness(context.Background())

	h.mu.Lock()
	before := h.failures
	h.mu.Unlock()

	for range 5 {
		get(t, h, "127.0.0.1:1234")
	}

	h.mu.Lock()
	after := h.failures
	h.mu.Unlock()
	if after != before {
		t.Errorf("failures went %d -> %d across 5 health requests: the handler is probing", before, after)
	}
}

// Draining must report unhealthy immediately, before the drain starts, so new
// connections stop arriving while there is still time to finish the old ones.
func TestHealthReportsDrainingImmediately(t *testing.T) {
	h := testHealth(t, nil)
	h.setDraining()

	code, body := get(t, h, "127.0.0.1:1234")
	if code != http.StatusServiceUnavailable || body.Status != "draining" {
		t.Errorf("status %d %q while draining, want 503 draining", code, body.Status)
	}
}

// The endpoint reports version and connection counts; it is not public.
func TestHealthRefusesNonLoopback(t *testing.T) {
	h := testHealth(t, nil)

	code, _ := get(t, h, "203.0.113.7:40000")
	if code != http.StatusForbidden {
		t.Errorf("status %d for a non-loopback client, want 403", code)
	}
}

func TestHealthReportsVersionAndCounts(t *testing.T) {
	h := testHealth(t, nil)
	h.checkLiveness(context.Background())

	_, body := get(t, h, "127.0.0.1:1234")
	if body.Version != version || body.Commit != commit {
		t.Errorf("version/commit = %q/%q, want %q/%q", body.Version, body.Commit, version, commit)
	}
	// Reported from the proxy, not the config: if the two ever disagree, the
	// endpoint should show what is actually being enforced.
	if body.Connections.Max != 10 {
		t.Errorf("connections.max = %d, want the cap the proxy enforces", body.Connections.Max)
	}
}

func TestBackendSettingsAvailable(t *testing.T) {
	// reserved_connections is PG16+; leaving it out of the subtraction is what
	// makes a cap look safe when it is not.
	s := backendSettings{MaxConnections: 100, SuperuserReserved: 3, ReservedConnection: 5}
	if got := s.Available(); got != 92 {
		t.Errorf("Available() = %d, want 92", got)
	}
}
