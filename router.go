package pgmux

import (
	"context"
	"errors"
)

// ErrUserNotFound is returned when a user mapping cannot be found
var ErrUserNotFound = errors.New("user mapping not found")

// BackendConfig represents the configuration for a backend PostgreSQL server
type BackendConfig struct {
	Host string
	Port int
	User string
}

// Router defines the interface for routing PostgreSQL connections
type Router interface {
	// Route returns the backend configuration for a given username
	// Returns ErrUserNotFound if no mapping exists
	Route(ctx context.Context, username string) (*BackendConfig, error)
}