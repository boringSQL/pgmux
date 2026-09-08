package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the whole of pgmuxd's configuration. It comes from the
// environment: there is no config file, and nothing is reloaded.
type Config struct {
	Listen string

	BackendHost     string
	BackendPort     int
	BackendUser     string
	BackendDatabase string

	TLSCert string
	TLSKey  string

	// BackendTLS upgrades the hop to the backend. Off by default because the
	// intended deployment reaches PostgreSQL over loopback or a private link.
	BackendTLS bool

	MaxConnections      int
	MaxConnectionsPerIP int
	MaxMessageSize      int
	ClientIdleTimeout   time.Duration

	StatementTimeout time.Duration
	IdleTxTimeout    time.Duration

	HealthAddr   string
	DrainTimeout time.Duration
	LogLevel     slog.Level
}

// Defaults. DrainTimeout sits under Nomad's 5s default kill_timeout on
// purpose: a longer default would be SIGKILLed mid-drain on every deploy by a
// job that never mentions kill_timeout.
const (
	defaultBackendPort         = 5432
	defaultMaxConnections      = 100
	defaultMaxConnectionsPerIP = 5
	// 16 MiB (pgmux's own default) times MaxConnections is a lot of
	// attacker-controlled allocation for an endpoint nobody sends large
	// queries to.
	defaultMaxMessageSize    = 1 << 20
	defaultClientIdleTimeout = 5 * time.Minute
	defaultStatementTimeout  = 30 * time.Second
	defaultIdleTxTimeout     = 60 * time.Second
	defaultHealthAddr        = "127.0.0.1:8080"
	defaultDrainTimeout      = 4 * time.Second
	// maxMessageSizeCeiling exists because the size is per connection: the
	// default is chosen by reasoning about size times MaxConnections, so
	// leaving the override unbounded would undo that reasoning.
	maxMessageSizeCeiling = 64 << 20
)

// Load reads and validates the configuration.
//
// Every problem is collected rather than returned at the first one: a proxy
// that fails on one typo per restart wastes a deploy cycle each time. A proxy
// that starts and only fails on its first visitor is worse still, so anything
// checkable is checked here — including actually loading the keypair.
func Load() (*Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := &Config{
		Listen:          os.Getenv("PGMUXD_LISTEN"),
		BackendHost:     os.Getenv("PGMUXD_BACKEND_HOST"),
		BackendUser:     os.Getenv("PGMUXD_BACKEND_USER"),
		BackendDatabase: os.Getenv("PGMUXD_BACKEND_DATABASE"),
		TLSCert:         os.Getenv("PGMUXD_TLS_CERT"),
		TLSKey:          os.Getenv("PGMUXD_TLS_KEY"),
	}

	for _, required := range []struct{ name, value string }{
		{"PGMUXD_LISTEN", cfg.Listen},
		{"PGMUXD_BACKEND_HOST", cfg.BackendHost},
		{"PGMUXD_BACKEND_USER", cfg.BackendUser},
		// Required, not optional: libpq defaults dbname to the client's OS
		// username, so an unset value sends every visitor to a database named
		// after themselves.
		{"PGMUXD_BACKEND_DATABASE", cfg.BackendDatabase},
		// pgmuxd refuses to run without TLS. A typo here must not put a
		// plaintext PostgreSQL on the public internet.
		{"PGMUXD_TLS_CERT", cfg.TLSCert},
		{"PGMUXD_TLS_KEY", cfg.TLSKey},
	} {
		if required.value == "" {
			fail("%s is required", required.name)
		}
	}

	cfg.BackendPort = envInt("PGMUXD_BACKEND_PORT", defaultBackendPort, fail)
	cfg.MaxConnections = envInt("PGMUXD_MAX_CONNECTIONS", defaultMaxConnections, fail)
	cfg.MaxConnectionsPerIP = envInt("PGMUXD_MAX_CONNECTIONS_PER_IP", defaultMaxConnectionsPerIP, fail)
	cfg.MaxMessageSize = envBytes("PGMUXD_MAX_MESSAGE_SIZE", defaultMaxMessageSize, fail)
	cfg.ClientIdleTimeout = envDuration("PGMUXD_CLIENT_IDLE_TIMEOUT", defaultClientIdleTimeout, fail)
	cfg.StatementTimeout = envDuration("PGMUXD_STATEMENT_TIMEOUT", defaultStatementTimeout, fail)
	cfg.IdleTxTimeout = envDuration("PGMUXD_IDLE_TX_TIMEOUT", defaultIdleTxTimeout, fail)
	cfg.DrainTimeout = envDuration("PGMUXD_DRAIN_TIMEOUT", defaultDrainTimeout, fail)

	cfg.BackendTLS = os.Getenv("PGMUXD_BACKEND_TLS") == "1"

	// Debug is where every per-connection diagnostic lives; without a knob,
	// investigating a live endpoint means a rebuild.
	if raw := os.Getenv("PGMUXD_LOG_LEVEL"); raw != "" {
		if err := cfg.LogLevel.UnmarshalText([]byte(raw)); err != nil {
			fail("PGMUXD_LOG_LEVEL %q is not one of debug, info, warn, error", raw)
		}
	}

	cfg.HealthAddr = os.Getenv("PGMUXD_HEALTH_ADDR")
	if cfg.HealthAddr == "" {
		cfg.HealthAddr = defaultHealthAddr
	}

	if cfg.Listen != "" {
		if _, err := net.ResolveTCPAddr("tcp", cfg.Listen); err != nil {
			fail("PGMUXD_LISTEN %q is not a valid address: %v", cfg.Listen, err)
		}
	}
	if err := validateLoopback(cfg.HealthAddr); err != nil {
		fail("PGMUXD_HEALTH_ADDR %q: %v", cfg.HealthAddr, err)
	}

	if cfg.BackendPort < 1 || cfg.BackendPort > 65535 {
		fail("PGMUXD_BACKEND_PORT %d is not a valid port", cfg.BackendPort)
	}
	for _, positive := range []struct {
		name  string
		value int
	}{
		{"PGMUXD_MAX_CONNECTIONS", cfg.MaxConnections},
		{"PGMUXD_MAX_MESSAGE_SIZE", cfg.MaxMessageSize},
	} {
		if positive.value <= 0 {
			fail("%s must be greater than zero", positive.name)
		}
	}
	if cfg.MaxConnectionsPerIP < 0 {
		fail("PGMUXD_MAX_CONNECTIONS_PER_IP must not be negative")
	}
	if cfg.MaxMessageSize > maxMessageSizeCeiling {
		fail("PGMUXD_MAX_MESSAGE_SIZE must not exceed %d bytes: it is multiplied by PGMUXD_MAX_CONNECTIONS",
			maxMessageSizeCeiling)
	}
	for _, positive := range []struct {
		name  string
		value time.Duration
	}{
		{"PGMUXD_DRAIN_TIMEOUT", cfg.DrainTimeout},
		// Zero is not "no limit" for these — pgmux reads a non-positive idle
		// timeout as disabled, and PostgreSQL reads statement_timeout=0 the
		// same way, so an override of 0 silently removes the protection. A
		// negative value makes PostgreSQL reject every startup, which starts
		// cleanly and then fails every visitor.
		{"PGMUXD_CLIENT_IDLE_TIMEOUT", cfg.ClientIdleTimeout},
		{"PGMUXD_STATEMENT_TIMEOUT", cfg.StatementTimeout},
		{"PGMUXD_IDLE_TX_TIMEOUT", cfg.IdleTxTimeout},
	} {
		if positive.value <= 0 {
			fail("%s must be greater than zero", positive.name)
		} else if positive.value < time.Millisecond {
			// The injected options are expressed in milliseconds, so anything
			// finer rounds to zero and disables the limit.
			fail("%s must be at least 1ms", positive.name)
		}
	}

	// Load the keypair here rather than letting Start do it, so an unreadable
	// key is reported alongside every other problem instead of one restart later.
	if cfg.TLSCert != "" && cfg.TLSKey != "" {
		if _, err := tls.LoadX509KeyPair(cfg.TLSCert, cfg.TLSKey); err != nil {
			fail("TLS keypair: %v", err)
		}
	}

	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return cfg, nil
}

// validateLoopback rejects a health address that is reachable from anywhere
// but this host: /healthz reports version and connection counts, and it must
// not become a public status page.
func validateLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("not a host:port address: %w", err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return errors.New("must be a loopback address")
	}
	if !ip.IsLoopback() {
		return errors.New("must be a loopback address")
	}
	return nil
}

func envInt(name string, fallback int, fail func(string, ...any)) int {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s %q is not a number", name, raw)
		return fallback
	}
	return n
}

func envDuration(name string, fallback time.Duration, fail func(string, ...any)) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		fail("%s %q is not a duration (try 30s, 5m)", name, raw)
		return fallback
	}
	return d
}

// envBytes accepts a plain byte count or a binary suffix, because 16777216 in
// a Nomad job file is not something anyone should have to read.
func envBytes(name string, fallback int, fail func(string, ...any)) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}

	multiplier := 1
	for suffix, factor := range map[string]int{"KiB": 1 << 10, "MiB": 1 << 20, "GiB": 1 << 30} {
		if strings.HasSuffix(raw, suffix) {
			multiplier = factor
			raw = strings.TrimSpace(strings.TrimSuffix(raw, suffix))
			break
		}
	}

	n, err := strconv.Atoi(raw)
	if err != nil {
		fail("%s is not a size (try 1048576 or 1MiB)", name)
		return fallback
	}
	if n < 0 || n > maxMessageSizeCeiling/multiplier {
		fail("%s is out of range", name)
		return fallback
	}
	return n * multiplier
}
