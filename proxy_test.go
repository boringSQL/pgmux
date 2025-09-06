package pgmux

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"
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