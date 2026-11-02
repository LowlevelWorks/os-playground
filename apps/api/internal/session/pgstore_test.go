package session_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/os-playground-api/internal/database"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/session/storetest"
)

// TestPGStoreContract runs the Store contract against a real, migrated
// database; see testPool.
func TestPGStoreContract(t *testing.T) {
	pool := testPool(t)
	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}

	storetest.Run(t, func(t *testing.T) session.Store {
		if _, err := pool.Exec(context.Background(), "TRUNCATE playground_sessions"); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return store
	})
}

// ADR 0010, section 9: an audit row cannot be changed or removed, even by
// the application's own connection.
func TestAuditEventsAreAppendOnly(t *testing.T) {
	pool := testPool(t)
	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := store.Admit(ctx, session.NewSession{
		OwnerID: "owner-audit", ClientKey: "192.0.2.1", Profile: "p", ImageRef: "i",
		Spec: []byte("{}"), MaxDuration: time.Hour, IdleTimeout: time.Minute,
	}, session.Limits{Capacity: 1000, PerOwner: 1000, PerClient: 1000})
	if err != nil {
		t.Fatal(err)
	}

	for _, stmt := range []string{
		"UPDATE audit_events SET event_type = 'X' WHERE session_id = $1",
		"DELETE FROM audit_events WHERE session_id = $1",
	} {
		if _, err := pool.Exec(ctx, stmt, s.ID); err == nil {
			t.Errorf("%q succeeded, want the append-only trigger to refuse it", stmt)
		}
	}
	if _, err := pool.Exec(ctx, "TRUNCATE audit_events"); err == nil {
		t.Error("TRUNCATE audit_events succeeded, want it refused")
	}
}

// testPool connects to the test database, or skips: locally without
// PLAYGROUND_TEST_DATABASE_URL. In CI a missing URL fails, so these tests can
// never be skipped there by accident. Tests TRUNCATE playground_sessions:
// never point it at a database you care about.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	url := os.Getenv("PLAYGROUND_TEST_DATABASE_URL")
	if url == "" {
		if os.Getenv("CI") != "" {
			t.Fatal("PLAYGROUND_TEST_DATABASE_URL is not set in CI")
		}
		t.Skip("PLAYGROUND_TEST_DATABASE_URL not set")
	}
	// Fewer connections than the 50 concurrent Admits in the contract, so
	// they queue for both the pool and the lock, as under real load.
	pool, err := database.New(context.Background(), url, 10)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}
