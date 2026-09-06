package pgmux

import (
	"context"
	"crypto/tls"
	"errors"
)

// ErrUserNotFound is returned when a user mapping cannot be found
var ErrUserNotFound = errors.New("user mapping not found")

type (
	// BackendConfig represents the configuration for a backend PostgreSQL server.
	//
	// Treat it as immutable once handed to a Router: the same pointer may be
	// returned to many concurrent connections.
	BackendConfig struct {
		Host string
		Port int
		User string
		// Database, when non-empty, replaces the client's requested database
		// in the StartupMessage. Empty passes the client's value through.
		//
		// libpq defaults dbname to the client's OS username when -d is omitted,
		// so a public endpoint that wants every visitor in one database must
		// set this.
		Database string
		// TLS, when non-nil, makes pgmux issue a PostgreSQL SSLRequest before
		// the StartupMessage and upgrade the backend connection to TLS using
		// this config. If the backend refuses SSL, the connection is rejected
		// rather than silently downgraded — the goal is to keep the SCRAM
		// exchange off the wire, so a downgrade defeats the point.
		TLS *tls.Config
	}

	// Router defines the interface for routing PostgreSQL connections
	Router interface {
		// Route returns the backend configuration for a given username
		// Returns ErrUserNotFound if no mapping exists
		Route(ctx context.Context, username string) (*BackendConfig, error)
	}
)
