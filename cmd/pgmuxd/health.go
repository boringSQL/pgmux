package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/boringsql/pgmux"
)

const (
	// probeTimeout bounds a single backend probe.
	probeTimeout = 2 * time.Second
	// probeInterval is how often the backend is probed. Consul polls every
	// ~10s; probing on a timer rather than per request keeps a burst of health
	// requests from turning into a burst of backend connections, and stops a
	// caller who hangs up mid-request from counting as a backend failure.
	probeInterval = 5 * time.Second
	// settingsInterval is how often the connection ceilings are re-read.
	// max_connections cannot change without a backend restart.
	settingsInterval = 5 * time.Minute
	// unhealthyAfter is how many consecutive probe failures it takes to report
	// unhealthy. A single blip must not deregister an endpoint whose existing
	// sessions are fine and whose database is merely busy.
	unhealthyAfter = 3
)

type health struct {
	cfg    *Config
	proxy  *pgmux.ProxyServer
	logger *slog.Logger
	server *http.Server

	mu          sync.Mutex
	draining    bool
	probed      bool
	failures    int
	lastErr     error
	lastLatency time.Duration
	settings    backendSettings
	settingsOK  bool
}

type healthResponse struct {
	Status      string          `json:"status"`
	Version     string          `json:"version"`
	Commit      string          `json:"commit"`
	BuildDate   string          `json:"build_date"`
	Backend     backendReport   `json:"backend"`
	Connections connectionStats `json:"connections"`
}

type backendReport struct {
	Reachable          bool   `json:"reachable"`
	LatencyMS          int64  `json:"latency_ms"`
	Error              string `json:"error,omitempty"`
	MaxConnections     int    `json:"max_connections,omitempty"`
	SuperuserReserved  int    `json:"superuser_reserved_connections,omitempty"`
	ReservedConnection int    `json:"reserved_connections,omitempty"`
}

type connectionStats struct {
	Current       int    `json:"current"`
	Accepted      uint64 `json:"accepted"`
	Rejected      uint64 `json:"rejected"`
	RejectedPerIP uint64 `json:"rejected_per_ip"`
	Max           int    `json:"max"`
}

func newHealth(cfg *Config, proxy *pgmux.ProxyServer, logger *slog.Logger) *health {
	h := &health{cfg: cfg, proxy: proxy, logger: logger}

	// Its own mux, never http.DefaultServeMux: any transitive net/http/pprof
	// import would otherwise publish the profiler on this listener.
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", h.handle)
	h.server = &http.Server{
		Addr:              cfg.HealthAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		// Without this a client that stops reading parks the handler, and
		// Shutdown then waits on it for as long as the client cares to hold on.
		WriteTimeout: 10 * time.Second,
	}
	return h
}

func (h *health) ListenAndServe() error { return h.server.ListenAndServe() }

func (h *health) Shutdown(ctx context.Context) error { return h.server.Shutdown(ctx) }

// setDraining flips /healthz to unhealthy. Called before the drain starts.
func (h *health) setDraining() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.draining = true
}

// Watch probes the backend on a timer and refreshes the connection ceilings
// less often. Everything /healthz reports comes from here, so serving a health
// request costs nothing and cannot be used to drive backend connections.
func (h *health) Watch(ctx context.Context) {
	h.checkLiveness(ctx)
	h.refreshSettings(ctx)

	liveness := time.NewTicker(probeInterval)
	defer liveness.Stop()
	settings := time.NewTicker(settingsInterval)
	defer settings.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-liveness.C:
			h.checkLiveness(ctx)
		case <-settings.C:
			h.refreshSettings(ctx)
		}
	}
}

func (h *health) checkLiveness(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	started := time.Now()
	_, err := probe(probeCtx, h.cfg, false)
	latency := time.Since(started)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.probed = true
	h.lastErr = err
	h.lastLatency = latency
	if err != nil {
		h.failures++
		if h.failures == unhealthyAfter {
			h.logger.Error("backend unreachable, reporting unhealthy",
				"failures", h.failures, "error", err)
		}
		return
	}
	if h.failures >= unhealthyAfter {
		h.logger.Info("backend reachable again")
	}
	h.failures = 0
}

// refreshSettings reads the backend's connection ceilings and logs how they
// compare to ours, so the number in the job file and the number in
// postgresql.conf cannot drift apart unnoticed.
func (h *health) refreshSettings(ctx context.Context) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	settings, err := probe(probeCtx, h.cfg, true)
	if err != nil {
		// Not fatal: Nomad gives no ordering guarantee, and the proxy may
		// legitimately come up before its database.
		h.logger.Warn("could not read backend connection settings", "error", err)
		return
	}

	h.mu.Lock()
	h.settings, h.settingsOK = settings, true
	h.mu.Unlock()

	available := settings.Available()
	attrs := []any{
		"proxy_max_connections", h.cfg.MaxConnections,
		"backend_max_connections", settings.MaxConnections,
		"superuser_reserved", settings.SuperuserReserved,
		"reserved", settings.ReservedConnection,
		"available_to_clients", available,
	}
	if h.cfg.MaxConnections > available {
		h.logger.Warn("proxy connection cap exceeds what the backend can serve", attrs...)
		return
	}
	h.logger.Info("connection caps", attrs...)
}

func (h *health) handle(w http.ResponseWriter, r *http.Request) {
	// The listener is bound to loopback; this is the second lock on the same
	// door, in case it is ever bound elsewhere by mistake.
	if !isLoopbackRequest(r) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	status, body := h.report()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (h *health) report() (int, healthResponse) {
	stats := h.proxy.Stats()
	body := healthResponse{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
		Connections: connectionStats{
			Current:       stats.Current,
			Accepted:      stats.Accepted,
			Rejected:      stats.Rejected,
			RejectedPerIP: stats.RejectedPerIP,
			Max:           stats.Max,
		},
	}

	h.mu.Lock()
	draining, probed, failures := h.draining, h.probed, h.failures
	lastErr, latency := h.lastErr, h.lastLatency
	if h.settingsOK {
		body.Backend.MaxConnections = h.settings.MaxConnections
		body.Backend.SuperuserReserved = h.settings.SuperuserReserved
		body.Backend.ReservedConnection = h.settings.ReservedConnection
	}
	h.mu.Unlock()

	body.Backend.LatencyMS = latency.Milliseconds()
	body.Backend.Reachable = probed && lastErr == nil
	if lastErr != nil {
		body.Backend.Error = lastErr.Error()
	}

	switch {
	case draining:
		body.Status = "draining"
		return http.StatusServiceUnavailable, body

	// Reporting ok before the listener exists would have Consul advertise an
	// endpoint nothing is listening on.
	case !probed || h.proxy.Addr() == nil:
		body.Status = "starting"
		return http.StatusServiceUnavailable, body

	case failures >= unhealthyAfter:
		body.Status = "unhealthy"
		return http.StatusServiceUnavailable, body

	case lastErr != nil:
		// Below the threshold the endpoint stays in service: existing sessions
		// are unaffected, and flapping out of Consul on one blip is worse.
		body.Status = "degraded"
		return http.StatusOK, body

	case stats.Max > 0 && stats.Current >= stats.Max:
		// Still 200: with a single instance, deregistering a full proxy denies
		// everyone rather than the few being refused.
		body.Status = "saturated"
		return http.StatusOK, body
	}

	body.Status = "ok"
	return http.StatusOK, body
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
