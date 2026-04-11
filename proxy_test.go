package pgmux

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"github.com/jackc/pgproto3/v2"
)

// mockRouter implements Router for testing
type mockRouter struct {
	routes map[string]*BackendConfig
}

func (m *mockRouter) Route(ctx context.Context, username string) (*BackendConfig, error) {
	if config, ok := m.routes[username]; ok {
		return config, nil
	}
	return nil, ErrUserNotFound
}

func TestProxyServerStart(t *testing.T) {
	router := &mockRouter{
		routes: map[string]*BackendConfig{
			"test_user": {
				Host: "localhost",
				Port: 5432,
				User: "postgres",
			},
		},
	}

	proxy := NewProxyServer("127.0.0.1:0", router) // Use port 0 for random port
	
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Start proxy in background
	errChan := make(chan error, 1)
	go func() {
		errChan <- proxy.Start(ctx)
	}()

	// Give proxy time to start
	time.Sleep(100 * time.Millisecond)

	// Try to connect
	conn, err := net.Dial("tcp", proxy.listenAddr)
	if err == nil {
		conn.Close()
		t.Error("Expected connection to fail without proper PostgreSQL handshake")
	}

	// Shutdown
	cancel()
	
	select {
	case err := <-errChan:
		if err != nil {
			t.Errorf("Start() returned error: %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Error("Proxy didn't shut down in time")
	}
}

func TestProxyServerInvalidPort(t *testing.T) {
	router := &mockRouter{}
	proxy := NewProxyServer("invalid:port", router)
	
	ctx := context.Background()
	err := proxy.Start(ctx)
	
	if err == nil {
		t.Error("Expected error for invalid listen address")
	}
}

func TestProxyServerMaxConnectionsDefault(t *testing.T) {
	router := &mockRouter{}
	p := NewProxyServer("127.0.0.1:0", router)

	if got := p.maxConnections(); got != defaultMaxConnections {
		t.Errorf("unconfigured: expected %d, got %d", defaultMaxConnections, got)
	}

	p.WithLimits(&Limits{MaxConnections: 0})
	if got := p.maxConnections(); got != defaultMaxConnections {
		t.Errorf("zero: expected %d, got %d", defaultMaxConnections, got)
	}

	p.WithLimits(&Limits{MaxConnections: -5})
	if got := p.maxConnections(); got != defaultMaxConnections {
		t.Errorf("negative: expected %d, got %d", defaultMaxConnections, got)
	}

	p.WithLimits(&Limits{MaxConnections: 50})
	if got := p.maxConnections(); got != 50 {
		t.Errorf("custom: expected 50, got %d", got)
	}
}

func TestProxyServerMaxConnections(t *testing.T) {
	addr := pickFreePort(t)

	router := &mockRouter{routes: map[string]*BackendConfig{}}
	proxy := NewProxyServer(addr, router).WithLimits(&Limits{MaxConnections: 2})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errChan := make(chan error, 1)
	go func() { errChan <- proxy.Start(ctx) }()

	conn1 := dialUntilReady(t, addr)
	defer conn1.Close()

	conn2, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("conn2 dial: %v", err)
	}
	defer conn2.Close()

	// Let both handlers reach ReceiveStartupMessage so the semaphore is full.
	time.Sleep(100 * time.Millisecond)

	// Third connection should be rejected with SQLSTATE 53300.
	conn3, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("conn3 dial: %v", err)
	}
	defer conn3.Close()
	assertRejectedOverCapacity(t, conn3)

	// Free a slot and confirm a new connection is accepted.
	conn1.Close()
	time.Sleep(100 * time.Millisecond)

	conn4, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("conn4 dial: %v", err)
	}
	defer conn4.Close()
	assertNotRejected(t, conn4)

	cancel()
	select {
	case <-errChan:
	case <-time.After(2 * time.Second):
		t.Error("proxy didn't shut down in time")
	}
}

func pickFreePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pick free port: %v", err)
	}
	addr := l.Addr().String()
	l.Close()
	return addr
}

func dialUntilReady(t *testing.T, addr string) net.Conn {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			return conn
		}
		if time.Now().After(deadline) {
			t.Fatalf("proxy never became reachable at %s: %v", addr, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertRejectedOverCapacity(t *testing.T, conn net.Conn) {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	frontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	msg, err := frontend.Receive()
	if err != nil {
		t.Fatalf("expected ErrorResponse, got receive error: %v", err)
	}
	errResp, ok := msg.(*pgproto3.ErrorResponse)
	if !ok {
		t.Fatalf("expected *pgproto3.ErrorResponse, got %T", msg)
	}
	if errResp.Code != "53300" {
		t.Errorf("expected SQLSTATE 53300, got %q (message: %q)", errResp.Code, errResp.Message)
	}
}

func assertNotRejected(t *testing.T, conn net.Conn) {
	t.Helper()
	// The handler is blocked on ReceiveStartupMessage, so no bytes should arrive.
	// A read timeout is the success case; any 53300 ErrorResponse is a failure.
	conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	frontend := pgproto3.NewFrontend(pgproto3.NewChunkReader(conn), conn)
	msg, err := frontend.Receive()
	if err != nil {
		return // expected: read timeout
	}
	if errResp, ok := msg.(*pgproto3.ErrorResponse); ok && errResp.Code == "53300" {
		t.Errorf("expected connection to be accepted, got SQLSTATE 53300")
	}
}

func TestIsConnectionClosed(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{
			name:     "nil error",
			err:      nil,
			expected: false,
		},
		{
			name:     "regular error",
			err:      fmt.Errorf("some error"),
			expected: false,
		},
		{
			name: "read op error",
			err: &net.OpError{
				Op: "read",
			},
			expected: true,
		},
		{
			name: "write op error",
			err: &net.OpError{
				Op: "write",
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isConnectionClosed(tt.err)
			if result != tt.expected {
				t.Errorf("expected %v, got %v", tt.expected, result)
			}
		})
	}
}