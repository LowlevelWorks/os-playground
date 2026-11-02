package session_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/os-playground-api/internal/database"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/session/storetest"
)

// TestPGStoreContract runs the Store contract against a real, migrated
// database. Locally it is skipped without PLAYGROUND_TEST_DATABASE_URL; in CI
// a missing URL fails, so the test can never be skipped there by accident.
//
// It TRUNCATEs playground_sessions: never point it at a database you care about.
func TestPGStoreContract(t *testing.T) {
	url := os.Getenv("PLAYGROUND_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("PLAYGROUND_TEST_DATABASE_URL is not set in CI")
		}
		t.Skip("PLAYGROUND_TEST_DATABASE_URL not set")
	}

	ctx := context.Background()
	// Fewer connections than the 50 concurrent Admits, so they queue for both
	// the pool and the lock, as under real load.
	pool, err := database.New(ctx, url, 10)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	storetest.Run(t, func(t *testing.T) session.Store {
		if _, err := pool.Exec(ctx, "TRUNCATE playground_sessions"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return store
	})
}
