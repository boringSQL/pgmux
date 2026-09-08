package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeKeypair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	dir := t.TempDir()
	certFile = filepath.Join(dir, "server.crt")
	keyFile = filepath.Join(dir, "server.key")
	os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600)
	os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key),
	}), 0o600)
	return certFile, keyFile
}

// validEnv is a configuration with nothing wrong with it; tests break one
// thing at a time from here.
func validEnv(t *testing.T) map[string]string {
	t.Helper()
	certFile, keyFile := writeKeypair(t)
	return map[string]string{
		"PGMUXD_LISTEN":           "127.0.0.1:5432",
		"PGMUXD_BACKEND_HOST":     "10.0.0.1",
		"PGMUXD_BACKEND_USER":     "guest",
		"PGMUXD_BACKEND_DATABASE": "showcase",
		"PGMUXD_TLS_CERT":         certFile,
		"PGMUXD_TLS_KEY":          keyFile,
	}
}

func loadWith(t *testing.T, env map[string]string) (*Config, error) {
	t.Helper()
	// Clear everything first: an inherited PGMUXD_* from the developer's shell
	// would quietly change what is under test.
	for _, kv := range os.Environ() {
		if name, _, ok := strings.Cut(kv, "="); ok && strings.HasPrefix(name, "PGMUXD_") {
			t.Setenv(name, "")
			os.Unsetenv(name)
		}
	}
	for name, value := range env {
		t.Setenv(name, value)
	}
	return Load()
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := loadWith(t, validEnv(t))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.BackendPort != defaultBackendPort {
		t.Errorf("BackendPort = %d, want %d", cfg.BackendPort, defaultBackendPort)
	}
	if cfg.MaxConnections != defaultMaxConnections {
		t.Errorf("MaxConnections = %d, want %d", cfg.MaxConnections, defaultMaxConnections)
	}
	if cfg.MaxConnectionsPerIP != defaultMaxConnectionsPerIP {
		t.Errorf("MaxConnectionsPerIP = %d, want %d", cfg.MaxConnectionsPerIP, defaultMaxConnectionsPerIP)
	}
	if cfg.HealthAddr != defaultHealthAddr {
		t.Errorf("HealthAddr = %q, want %q", cfg.HealthAddr, defaultHealthAddr)
	}
	// Nomad's default kill_timeout is 5s: a longer default drain would be
	// SIGKILLed mid-drain on every deploy of a job that never sets it.
	if cfg.DrainTimeout >= 5*time.Second {
		t.Errorf("DrainTimeout = %v, must stay under Nomad's 5s default kill_timeout", cfg.DrainTimeout)
	}
	if cfg.ClientIdleTimeout <= 0 {
		t.Error("ClientIdleTimeout must default to something: a public listener cannot leave it disabled")
	}
}

func TestLoadRejects(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(map[string]string)
		wantErr string
	}{
		{"no listen", func(e map[string]string) { delete(e, "PGMUXD_LISTEN") }, "PGMUXD_LISTEN is required"},
		{"no backend host", func(e map[string]string) { delete(e, "PGMUXD_BACKEND_HOST") }, "PGMUXD_BACKEND_HOST is required"},
		{"no backend user", func(e map[string]string) { delete(e, "PGMUXD_BACKEND_USER") }, "PGMUXD_BACKEND_USER is required"},
		// Not optional: libpq defaults dbname to the client's OS username.
		{"no backend database", func(e map[string]string) { delete(e, "PGMUXD_BACKEND_DATABASE") }, "PGMUXD_BACKEND_DATABASE is required"},
		{"no cert", func(e map[string]string) { delete(e, "PGMUXD_TLS_CERT") }, "PGMUXD_TLS_CERT is required"},
		{"no key", func(e map[string]string) { delete(e, "PGMUXD_TLS_KEY") }, "PGMUXD_TLS_KEY is required"},
		{"unreadable key", func(e map[string]string) { e["PGMUXD_TLS_KEY"] = "/nonexistent/server.key" }, "TLS keypair"},
		{"bad listen", func(e map[string]string) { e["PGMUXD_LISTEN"] = "not-an-address" }, "not a valid address"},
		{"bad port", func(e map[string]string) { e["PGMUXD_BACKEND_PORT"] = "sixty" }, "is not a number"},
		{"port out of range", func(e map[string]string) { e["PGMUXD_BACKEND_PORT"] = "70000" }, "not a valid port"},
		{"bad duration", func(e map[string]string) { e["PGMUXD_CLIENT_IDLE_TIMEOUT"] = "5 minutes" }, "is not a duration"},
		{"bad size", func(e map[string]string) { e["PGMUXD_MAX_MESSAGE_SIZE"] = "big" }, "is not a size"},
		{"zero connections", func(e map[string]string) { e["PGMUXD_MAX_CONNECTIONS"] = "0" }, "greater than zero"},
		{"zero drain", func(e map[string]string) { e["PGMUXD_DRAIN_TIMEOUT"] = "0s" }, "greater than zero"},
		// Zero is not "no limit": pgmux and PostgreSQL both read it as
		// disabled, so an override of 0 silently removes the protection.
		{"zero statement timeout", func(e map[string]string) { e["PGMUXD_STATEMENT_TIMEOUT"] = "0s" }, "PGMUXD_STATEMENT_TIMEOUT must be greater than zero"},
		{"zero idle timeout", func(e map[string]string) { e["PGMUXD_CLIENT_IDLE_TIMEOUT"] = "0" }, "PGMUXD_CLIENT_IDLE_TIMEOUT must be greater than zero"},
		{"zero idle tx timeout", func(e map[string]string) { e["PGMUXD_IDLE_TX_TIMEOUT"] = "0s" }, "PGMUXD_IDLE_TX_TIMEOUT must be greater than zero"},
		// PostgreSQL FATALs on a negative statement_timeout, so the proxy
		// would start cleanly and fail every visitor.
		{"negative statement timeout", func(e map[string]string) { e["PGMUXD_STATEMENT_TIMEOUT"] = "-1s" }, "PGMUXD_STATEMENT_TIMEOUT must be greater than zero"},
		// Rounds to 0 in the injected options, which means disabled.
		{"sub-millisecond timeout", func(e map[string]string) { e["PGMUXD_STATEMENT_TIMEOUT"] = "500us" }, "at least 1ms"},
		{"oversized message limit", func(e map[string]string) { e["PGMUXD_MAX_MESSAGE_SIZE"] = "512MiB" }, "out of range"},
		{"bad log level", func(e map[string]string) { e["PGMUXD_LOG_LEVEL"] = "chatty" }, "PGMUXD_LOG_LEVEL"},
		// /healthz reports version and connection counts; it is not a public
		// status page.
		{"public health address", func(e map[string]string) { e["PGMUXD_HEALTH_ADDR"] = "0.0.0.0:8080" }, "must be a loopback address"},
		{"health address without port", func(e map[string]string) { e["PGMUXD_HEALTH_ADDR"] = "127.0.0.1" }, "not a host:port"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := validEnv(t)
			tc.mutate(env)

			cfg, err := loadWith(t, env)
			if err == nil {
				t.Fatalf("Load accepted %s, got %+v", tc.name, cfg)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wantErr)
			}
		})
	}
}

// One run must surface every problem: fixing them one restart at a time wastes
// a deploy cycle each time.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	env := validEnv(t)
	delete(env, "PGMUXD_LISTEN")
	delete(env, "PGMUXD_BACKEND_USER")
	env["PGMUXD_BACKEND_PORT"] = "nope"

	_, err := loadWith(t, env)
	if err == nil {
		t.Fatal("Load accepted a configuration with three problems")
	}
	for _, want := range []string{"PGMUXD_LISTEN", "PGMUXD_BACKEND_USER", "PGMUXD_BACKEND_PORT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%s", want, err)
		}
	}
}

func TestLoadParsesOverrides(t *testing.T) {
	env := validEnv(t)
	env["PGMUXD_BACKEND_PORT"] = "5433"
	env["PGMUXD_MAX_CONNECTIONS"] = "42"
	env["PGMUXD_MAX_CONNECTIONS_PER_IP"] = "0"
	env["PGMUXD_MAX_MESSAGE_SIZE"] = "2MiB"
	env["PGMUXD_CLIENT_IDLE_TIMEOUT"] = "90s"
	env["PGMUXD_DRAIN_TIMEOUT"] = "25s"
	env["PGMUXD_HEALTH_ADDR"] = "localhost:9999"

	cfg, err := loadWith(t, env)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BackendPort != 5433 || cfg.MaxConnections != 42 {
		t.Errorf("got port=%d max=%d", cfg.BackendPort, cfg.MaxConnections)
	}
	// Zero is meaningful here — it disables the per-IP cap — so it must not be
	// mistaken for "unset" and replaced by the default.
	if cfg.MaxConnectionsPerIP != 0 {
		t.Errorf("MaxConnectionsPerIP = %d, want an explicit 0 to disable the cap", cfg.MaxConnectionsPerIP)
	}
	if cfg.MaxMessageSize != 2<<20 {
		t.Errorf("MaxMessageSize = %d, want 2MiB", cfg.MaxMessageSize)
	}
	if cfg.ClientIdleTimeout != 90*time.Second || cfg.DrainTimeout != 25*time.Second {
		t.Errorf("got idle=%v drain=%v", cfg.ClientIdleTimeout, cfg.DrainTimeout)
	}
}

// The injected options must reach the backend, and must not clobber a client's.
func TestInjectTimeoutsAppendsToClientOptions(t *testing.T) {
	cfg := &Config{StatementTimeout: 30 * time.Second, IdleTxTimeout: time.Minute}
	rewrite := injectTimeouts(cfg)

	params := map[string]string{"options": "-c application_name=mine"}
	rewrite(nil, params)

	got := params["options"]
	if !strings.Contains(got, "-c application_name=mine") {
		t.Errorf("options = %q, dropped the client's own", got)
	}
	if !strings.Contains(got, "statement_timeout=30000") {
		t.Errorf("options = %q, want statement_timeout in ms", got)
	}
	if !strings.Contains(got, "idle_in_transaction_session_timeout=60000") {
		t.Errorf("options = %q, want idle_in_transaction_session_timeout in ms", got)
	}

	empty := map[string]string{}
	rewrite(nil, empty)
	if !strings.Contains(empty["options"], "statement_timeout") {
		t.Errorf("options = %q with no client value", empty["options"])
	}
}
