package pgmux

import (
	"context"
	"testing"
)

func TestStaticRouter(t *testing.T) {
	ctx := context.Background()
	
	t.Run("empty router", func(t *testing.T) {
		router := NewStaticRouter(nil)
		_, err := router.Route(ctx, "any_user")
		if err != ErrUserNotFound {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})

	t.Run("basic routing", func(t *testing.T) {
		mappings := map[string]*BackendConfig{
			"user1": {
				Host: "host1.example.com",
				Port: 5432,
				User: "backend_user1",
			},
			"user2": {
				Host: "host2.example.com",
				Port: 5433,
				User: "backend_user2",
			},
		}
		
		router := NewStaticRouter(mappings)
		
		// Test user1
		config, err := router.Route(ctx, "user1")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.Host != "host1.example.com" || config.Port != 5432 || config.User != "backend_user1" {
			t.Errorf("unexpected config: %+v", config)
		}
		
		// Test user2
		config, err = router.Route(ctx, "user2")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.Host != "host2.example.com" || config.Port != 5433 || config.User != "backend_user2" {
			t.Errorf("unexpected config: %+v", config)
		}
		
		// Test unknown user
		_, err = router.Route(ctx, "unknown")
		if err != ErrUserNotFound {
			t.Errorf("expected ErrUserNotFound, got %v", err)
		}
	})

	t.Run("add and remove mappings", func(t *testing.T) {
		router := NewStaticRouter(nil)
		
		// Add mapping
		config := &BackendConfig{
			Host: "new.example.com",
			Port: 5432,
			User: "new_backend",
		}
		router.AddMapping("new_user", config)
		
		// Verify it exists
		result, err := router.Route(ctx, "new_user")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if result != config {
			t.Error("expected same config pointer")
		}
		
		// Remove mapping
		router.RemoveMapping("new_user")
		
		// Verify it's gone
		_, err = router.Route(ctx, "new_user")
		if err != ErrUserNotFound {
			t.Errorf("expected ErrUserNotFound after removal, got %v", err)
		}
	})
}