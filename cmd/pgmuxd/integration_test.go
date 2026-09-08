//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Pinned by digest, the same discipline the Nomad artifact block uses for the
// binary: "postgres:17-alpine" is a moving target.
const postgresImage = "postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73"

// requireTools skips rather than fails: this suite must stay runnable on a
// laptop without docker.
// requireTools skips rather than fails so this stays runnable on a laptop
// without docker — unless PGMUXD_INTEGRATION_STRICT is set, which CI should
// set, because a skipped suite otherwise reads as a green one.
func requireTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"docker", "psql"} {
		if _, err := exec.LookPath(tool); err != nil {
			unavailable(t, "%s not on PATH", tool)
		}
	}
}

func unavailable(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("PGMUXD_INTEGRATION_STRICT") != "" {
		t.Fatalf(format, args...)
	}
	t.Skipf(format, args...)
}

// psqlEnv builds the child environment explicitly. Inheriting os.Environ()
// would let a PGUSER or PGDATABASE in the developer's shell silently satisfy
// the assertion this whole test exists to make.
func psqlEnv() []string {
	return []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
}

func startPostgres(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "-d", "-P",
		"-e", "POSTGRES_HOST_AUTH_METHOD=trust",
		"-e", "POSTGRES_PASSWORD=unused",
		postgresImage).CombinedOutput()
	if err != nil {
		unavailable(t, "could not start postgres (%v): %s", err, out)
	}
	id := strings.TrimSpace(string(out))
	t.Cleanup(func() { exec.Command("docker", "rm", "-f", id).Run() })

	portOut, err := exec.Command("docker", "port", id, "5432/tcp").Output()
	if err != nil {
		t.Fatalf("docker port: %v", err)
	}
	_, port, err := net.SplitHostPort(strings.TrimSpace(strings.Split(string(portOut), "\n")[0]))
	if err != nil {
		t.Fatalf("parse mapped port %q: %v", portOut, err)
	}

	// Retry a real query rather than pg_isready: it is in the client package
	// we may not have, and it reports ready before the entrypoint's init phase
	// has finished creating the database.
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		cmd := exec.Command("psql", "-h", "127.0.0.1", "-p", port, "-U", "postgres",
			"-d", "postgres", "-Atc", "select 1")
		cmd.Env = psqlEnv()
		if err := cmd.Run(); err == nil {
			return port
		}
		time.Sleep(time.Second)
	}
	t.Fatal("postgres never became ready")
	return ""
}

func runSQL(t *testing.T, port, sql string) {
	t.Helper()
	cmd := exec.Command("psql", "-h", "127.0.0.1", "-p", port, "-U", "postgres",
		"-d", "postgres", "-v", "ON_ERROR_STOP=1", "-Atc", sql)
	cmd.Env = psqlEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("psql %q: %v\n%s", sql, err, out)
	}
}

// query runs psql against the proxy with NO -U and NO -d: libpq then sends the
// local OS username for both, which is the whole point of the assertion.
func query(t *testing.T, proxyPort, conninfo, sql string) (string, error) {
	t.Helper()
	cmd := exec.Command("psql", fmt.Sprintf("host=127.0.0.1 port=%s %s", proxyPort, conninfo),
		"-Atc", sql)
	cmd.Env = psqlEnv()
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func startProxy(t *testing.T, backendPort string) (proxyPort, healthPort string, proc *exec.Cmd) {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "pgmuxd")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}

	certFile, keyFile := writeKeypair(t)
	proxyPort, healthPort = freePort(t), freePort(t)

	proc = exec.Command(binary)
	proc.Env = []string{
		"PGMUXD_LISTEN=127.0.0.1:" + proxyPort,
		"PGMUXD_BACKEND_HOST=127.0.0.1",
		"PGMUXD_BACKEND_PORT=" + backendPort,
		"PGMUXD_BACKEND_USER=guest",
		"PGMUXD_BACKEND_DATABASE=showcase",
		"PGMUXD_TLS_CERT=" + certFile,
		"PGMUXD_TLS_KEY=" + keyFile,
		"PGMUXD_HEALTH_ADDR=127.0.0.1:" + healthPort,
		"PGMUXD_DRAIN_TIMEOUT=10s",
	}
	proc.Stdout, proc.Stderr = os.Stderr, os.Stderr
	if err := proc.Start(); err != nil {
		t.Fatalf("start pgmuxd: %v", err)
	}
	t.Cleanup(func() {
		proc.Process.Kill()
		proc.Wait()
	})

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.Dial("tcp", "127.0.0.1:"+proxyPort); err == nil {
			c.Close()
			return proxyPort, healthPort, proc
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("pgmuxd never started listening")
	return "", "", nil
}

// The whole feature, end to end: psql with no -U and no -d must land on the
// configured role and database rather than on the caller's OS username.
func TestEndToEnd(t *testing.T) {
	requireTools(t)
	backendPort := startPostgres(t)
	runSQL(t, backendPort, "create role guest login")
	runSQL(t, backendPort, "create database showcase owner guest")

	proxyPort, healthPort, proc := startProxy(t, backendPort)

	t.Run("identity is rewritten", func(t *testing.T) {
		got, err := query(t, proxyPort, "sslmode=require", "select current_user, current_database()")
		if err != nil {
			t.Fatalf("psql: %v\n%s", err, got)
		}
		if got != "guest|showcase" {
			t.Errorf("got %q, want guest|showcase", got)
		}
	})

	// The failure mode this binary exists to prevent.
	t.Run("plaintext is refused", func(t *testing.T) {
		got, err := query(t, proxyPort, "sslmode=disable", "select 1")
		if err == nil {
			t.Fatalf("a plaintext connection succeeded: %q", got)
		}
		if !strings.Contains(got, "SSL connection is required") {
			t.Errorf("output = %q, want the SSL requirement", got)
		}
	})

	// libpq's default gssencmode takes a different path into the proxy.
	t.Run("plaintext after GSS is refused", func(t *testing.T) {
		got, err := query(t, proxyPort, "sslmode=disable gssencmode=prefer", "select 1")
		if err == nil {
			t.Fatalf("a plaintext connection succeeded via GSS: %q", got)
		}
		if !strings.Contains(got, "SSL connection is required") {
			t.Errorf("output = %q, want the SSL requirement", got)
		}
	})

	t.Run("injected statement_timeout applies", func(t *testing.T) {
		got, err := query(t, proxyPort, "sslmode=require", "show statement_timeout")
		if err != nil {
			t.Fatalf("psql: %v\n%s", err, got)
		}
		// The default is 30s; "not zero" would also pass on 1ms.
		if got != "30s" {
			t.Errorf("statement_timeout = %q, want 30s", got)
		}
	})

	t.Run("healthz reports a live backend", func(t *testing.T) {
		resp, err := http.Get("http://127.0.0.1:" + healthPort + "/healthz")
		if err != nil {
			t.Fatalf("GET /healthz: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("status %d, want 200", resp.StatusCode)
		}

		var body healthResponse
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !body.Backend.Reachable {
			t.Errorf("backend unreachable: %s", body.Backend.Error)
		}
		// Not Current: whether an earlier psql has been reaped is a race.
		if body.Connections.Accepted < 1 {
			t.Errorf("accepted = %d, want at least one", body.Connections.Accepted)
		}
		if body.Connections.Rejected != 0 {
			t.Errorf("rejected = %d, want 0", body.Connections.Rejected)
		}
	})

	// The point of the Background-rooted lifetime context in main.go: a
	// session in flight when SIGTERM arrives must finish, not be severed.
	// Handing Start the signal context instead would make the drain a no-op
	// that always reports success, and nothing else would notice.
	t.Run("sigterm drains an in-flight query", func(t *testing.T) {
		type result struct {
			out string
			err error
		}
		results := make(chan result, 1)
		go func() {
			out, err := query(t, proxyPort, "sslmode=require",
				"select pg_sleep(3), current_user")
			results <- result{out, err}
		}()

		// Let the query reach the backend before signalling.
		time.Sleep(1500 * time.Millisecond)
		if err := proc.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatalf("signal: %v", err)
		}

		select {
		case got := <-results:
			if got.err != nil {
				t.Fatalf("in-flight query was cut off by the drain: %v\n%s", got.err, got.out)
			}
			if !strings.Contains(got.out, "guest") {
				t.Errorf("query returned %q, want the completed row", got.out)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("in-flight query never returned")
		}

		done := make(chan error, 1)
		go func() { done <- proc.Wait() }()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("exited with %v, want a clean exit", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("did not exit after SIGTERM")
		}
	})

}
