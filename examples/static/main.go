package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/boringsql/pgmux"
)

func createExampleRouter() pgmux.Router {
	// Create a static router with a hardcoded example mapping
	// In a real application, you would implement your own Router interface
	mappings := map[string]*pgmux.BackendConfig{
		"app_user": {
			Host: "10.0.1.50",
			Port: 5432,
			User: "postgres",
		},
		"readonly_user": {
			Host: "10.0.1.51",
			Port: 5432,
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

	// Create and start proxy server
	proxy := pgmux.NewProxyServer(listenAddr, router)
	log.Fatal(proxy.Start(ctx))
}
