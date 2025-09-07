package main

import (
	"context"
	"crypto/tls"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/boringsql/pgmux"
)

func createExampleRouter() pgmux.Router {
	// Create a static router with example mappings
	mappings := map[string]*pgmux.BackendConfig{
		"app_user": {
			Host: "localhost",
			Port: 5432,
			User: "postgres",
		},
		"readonly_user": {
			Host: "localhost",
			Port: 5433,
			User: "readonly",
		},
	}
	return pgmux.NewStaticRouter(mappings)
}

func main() {
	listenAddr := ":5434"
	if len(os.Args) > 1 {
		listenAddr = os.Args[1]
	}

	// TLS certificate and key paths
	certFile := os.Getenv("TLS_CERT")
	keyFile := os.Getenv("TLS_KEY")
	
	if certFile == "" {
		certFile = "server.crt"
	}
	if keyFile == "" {
		keyFile = "server.key"
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down proxy server...")
		cancel()
	}()

	// Create router with example mappings
	router := createExampleRouter()

	// Create proxy server with TLS support
	proxy := pgmux.NewProxyServer(listenAddr, router)
	
	// Configure TLS
	tlsConfig := &pgmux.TLSConfig{
		Enabled:  true,
		CertFile: certFile,
		KeyFile:  keyFile,
	}
	
	// Optionally configure advanced TLS settings
	if os.Getenv("TLS_MIN_VERSION") == "1.3" {
		tlsConfig.Config = &tls.Config{
			MinVersion: tls.VersionTLS13,
		}
		// Load certificates into the custom config
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			log.Fatalf("Failed to load certificates: %v", err)
		}
		tlsConfig.Config.Certificates = []tls.Certificate{cert}
	}
	
	proxy.WithTLS(tlsConfig)
	
	log.Printf("Starting PostgreSQL TLS proxy on %s", listenAddr)
	log.Printf("Using certificate: %s", certFile)
	log.Printf("Using key: %s", keyFile)
	
	log.Fatal(proxy.Start(ctx))
}