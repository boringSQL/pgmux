package pgmux

import (
	"context"
	"crypto/tls"
	"testing"
	"time"
)

func TestProxyServerWithTLS(t *testing.T) {
	// Create a simple router for testing
	router := NewStaticRouter(map[string]*BackendConfig{
		"testuser": {
			Host: "localhost",
			Port: 5432,
			User: "postgres",
		},
	})

	// Test creating proxy with TLS configuration
	proxy := NewProxyServer(":0", router)

	tlsConfig := &TLSConfig{
		Enabled: true,
		Config: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
	}

	result := proxy.WithTLS(tlsConfig)

	// Verify the proxy is returned for chaining
	if result != proxy {
		t.Error("WithTLS should return the proxy instance for chaining")
	}

	// Verify TLS config is set
	if proxy.tlsConfig == nil {
		t.Error("TLS config should be set")
	}

	if !proxy.tlsConfig.Enabled {
		t.Error("TLS should be enabled")
	}

	if proxy.tlsConfig.Config.MinVersion != tls.VersionTLS12 {
		t.Error("TLS min version should be TLS 1.2")
	}
}

func TestTLSConfigValidation(t *testing.T) {
	router := NewStaticRouter(map[string]*BackendConfig{})
	proxy := NewProxyServer(":0", router)

	// Test with cert/key files
	tlsConfig := &TLSConfig{
		Enabled:  true,
		CertFile: "test.crt",
		KeyFile:  "test.key",
	}

	proxy.WithTLS(tlsConfig)

	if proxy.tlsConfig.CertFile != "test.crt" {
		t.Error("Certificate file should be set")
	}

	if proxy.tlsConfig.KeyFile != "test.key" {
		t.Error("Key file should be set")
	}
}

func TestProxyServerStartWithInvalidTLS(t *testing.T) {
	router := NewStaticRouter(map[string]*BackendConfig{})
	proxy := NewProxyServer(":0", router)

	// Configure TLS without certificates
	tlsConfig := &TLSConfig{
		Enabled: true,
		// No certificates provided
	}
	proxy.WithTLS(tlsConfig)

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := proxy.Start(ctx)
	if err == nil {
		t.Error("Should fail when TLS is enabled without certificates")
	}

	expectedError := "TLS enabled but no certificates provided"
	if err.Error() != expectedError {
		t.Errorf("Expected error '%s', got '%s'", expectedError, err.Error())
	}
}
