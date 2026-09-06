package pgmux

import (
	"context"
	"sync"
)

// StaticRouter implements Router with a static mapping of users to backends
type StaticRouter struct {
	mappings map[string]*BackendConfig
	fallback *BackendConfig
	mu       sync.RWMutex
}

// NewStaticRouter creates a new StaticRouter with the provided mappings
func NewStaticRouter(mappings map[string]*BackendConfig) *StaticRouter {
	return &StaticRouter{
		mappings: mappings,
	}
}

// Route returns the backend configuration for the given username
func (r *StaticRouter) Route(ctx context.Context, username string) (*BackendConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	config, exists := r.mappings[username]
	if !exists {
		if r.fallback != nil {
			return r.fallback, nil
		}
		return nil, ErrUserNotFound
	}

	return config, nil
}

// WithFallback sets the backend used for usernames with no explicit mapping.
// Nil (the default) keeps the strict behaviour of returning ErrUserNotFound.
func (r *StaticRouter) WithFallback(config *BackendConfig) *StaticRouter {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.fallback = config
	return r
}

// AddMapping adds or updates a user mapping
func (r *StaticRouter) AddMapping(username string, config *BackendConfig) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.mappings == nil {
		r.mappings = make(map[string]*BackendConfig)
	}

	r.mappings[username] = config
}

// RemoveMapping removes a user mapping
func (r *StaticRouter) RemoveMapping(username string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.mappings, username)
}
