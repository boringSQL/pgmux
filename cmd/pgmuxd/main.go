package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/boringsql/pgmux"
)

func main() {
	resolveVersion()

	cfg, err := Load()
	if err != nil {
		// Plain text to stderr: this runs before the logger exists, and an
		// operator reading a crashed alloc wants the list, not JSON.
		fmt.Fprintln(os.Stderr, "pgmuxd: invalid configuration")
		for _, line := range strings.Split(err.Error(), "\n") {
			fmt.Fprintln(os.Stderr, "  -", line)
		}
		os.Exit(1)
	}

	os.Exit(run(cfg))
}

func run(cfg *Config) int {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: cfg.LogLevel}))
	logger.Info("pgmuxd starting",
		"version", version, "commit", commit, "build_date", buildDate,
		"listen", cfg.Listen, "health", cfg.HealthAddr,
		"backend", net.JoinHostPort(cfg.BackendHost, fmt.Sprint(cfg.BackendPort)),
		"backend_user", cfg.BackendUser, "backend_database", cfg.BackendDatabase,
		"max_connections", cfg.MaxConnections,
		"max_connections_per_ip", cfg.MaxConnectionsPerIP)

	if cfg.MaxConnectionsPerIP == 0 {
		logger.Warn("per-IP connection limit disabled: one source can occupy every slot")
	}

	if !cfg.BackendTLS && !isLoopbackHost(cfg.BackendHost) {
		logger.Warn("backend connection is not encrypted; set PGMUXD_BACKEND_TLS=1 unless the link is private",
			"backend_host", cfg.BackendHost)
	}

	proxy := newProxy(cfg, logger)

	// Rooted in Background, deliberately not in the signal context: the relay
	// loops observe cancellation between messages, so handing Start the signal
	// context would sever every live session the instant SIGTERM arrives and
	// turn the drain into a no-op that always reports success.
	lifetime, stopLifetime := context.WithCancel(context.Background())
	defer stopLifetime()

	h := newHealth(cfg, proxy, logger)
	go h.Watch(lifetime)
	healthErr := make(chan error, 1)
	go func() {
		err := h.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		healthErr <- err
	}()

	startErr := make(chan error, 1)
	go func() { startErr <- proxy.Start(lifetime) }()

	sigCtx, stopSignals := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stopSignals()

	select {
	case err := <-startErr:
		// A bind failure must not leave the process blocked on a signal it
		// will never receive, with a healthy /healthz and no listener.
		logger.Error("proxy stopped", "error", err)
		return 1
	case err := <-healthErr:
		logger.Error("health server stopped", "error", err)
		return 1
	case <-sigCtx.Done():
	}

	// Restores default signal handling, so a second SIGTERM kills a wedged
	// drain instead of being swallowed.
	stopSignals()

	// Before draining, not after: Consul should stop sending new connections
	// while there is still time to finish the ones in flight.
	h.setDraining()
	logger.Info("draining", "timeout", cfg.DrainTimeout, "open", proxy.Stats().Current)

	drainCtx, cancelDrain := context.WithTimeout(context.Background(), cfg.DrainTimeout)
	defer cancelDrain()

	if err := proxy.Shutdown(drainCtx); err != nil {
		// Still zero: SIGTERM is operator-initiated, and a non-zero exit on
		// every deploy feeds Nomad's restart and reschedule counters.
		logger.Warn("drain did not complete", "error", err)
	}
	stopLifetime()

	healthCtx, cancelHealth := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelHealth()
	if err := h.Shutdown(healthCtx); err != nil {
		logger.Warn("health server shutdown", "error", err)
	}
	<-startErr

	logger.Info("stopped", "accepted", proxy.Stats().Accepted)
	return 0
}

func newProxy(cfg *Config, logger *slog.Logger) *pgmux.ProxyServer {
	backend := &pgmux.BackendConfig{
		Host:     cfg.BackendHost,
		Port:     cfg.BackendPort,
		User:     cfg.BackendUser,
		Database: cfg.BackendDatabase,
	}
	if cfg.BackendTLS {
		backend.TLS = &tls.Config{ServerName: cfg.BackendHost, MinVersion: tls.VersionTLS12}
	}
	router := pgmux.NewStaticRouter(nil).WithFallback(backend)

	return pgmux.NewProxyServer(cfg.Listen, router).
		WithLogger(logger).
		WithTLS(&pgmux.TLSConfig{
			Enabled:  true,
			Required: true,
			CertFile: cfg.TLSCert,
			KeyFile:  cfg.TLSKey,
			Config:   &tls.Config{MinVersion: tls.VersionTLS12},
		}).
		WithLimits(&pgmux.Limits{
			MaxConnections:      cfg.MaxConnections,
			MaxConnectionsPerIP: cfg.MaxConnectionsPerIP,
			MaxMessageSize:      cfg.MaxMessageSize,
			ClientIdleTimeout:   cfg.ClientIdleTimeout,
		}).
		WithStartupRewrite(injectTimeouts(cfg))
}

// injectTimeouts sets per-session limits through the startup options, so they
// apply however the backend role is configured.
//
// These are defaults, not limits: both settings are USERSET, so a visitor can
// raise them with SET. They stop the accidental runaway query, not a determined
// one — MaxConnections, ClientIdleTimeout and backend-side supervision are what
// actually bound a hostile client.
func injectTimeouts(cfg *Config) pgmux.StartupRewriteFunc {
	options := fmt.Sprintf("-c statement_timeout=%d -c idle_in_transaction_session_timeout=%d",
		cfg.StatementTimeout.Milliseconds(), cfg.IdleTxTimeout.Milliseconds())

	return func(_ net.Addr, params map[string]string) {
		// Appended, so a client's own options survive; ours come last and win.
		if existing := params["options"]; existing != "" {
			params["options"] = existing + " " + options
			return
		}
		params["options"] = options
	}
}

// isLoopbackHost reports whether the backend is reachable without the
// connection leaving the machine.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return false
}
