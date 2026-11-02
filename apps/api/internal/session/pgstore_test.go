package session_test

import (
	"context"
	"errors"
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

// The retention_roles migration: only playground_retention may delete audit
// rows, nobody may change or truncate them, and the app role cannot delete
// them even through a mistaken grant.
func TestAuditRetentionRole(t *testing.T) {
	pool := testPool(t)
	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := store.Admit(ctx, session.NewSession{
		OwnerID: "owner-retention", ClientKey: "192.0.2.1", Profile: "p", ImageRef: "i",
		Spec: []byte("{}"), MaxDuration: time.Hour, IdleTimeout: time.Minute,
	}, session.Limits{Capacity: 1000, PerOwner: 1000, PerClient: 1000})
	if err != nil {
		t.Fatal(err)
	}

	// as runs stmt in a transaction as role, always rolled back.
	as := func(role, stmt string) error {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE "+role); err != nil {
			t.Fatal(err)
		}
		_, err = tx.Exec(ctx, stmt, s.ID)
		return err
	}
	del := "DELETE FROM audit_events WHERE session_id = $1"
	upd := "UPDATE audit_events SET event_type = 'X' WHERE session_id = $1"

	if err := as("playground_retention", del); err != nil {
		t.Errorf("retention role DELETE = %v, want allowed", err)
	}
	if err := as("playground_retention", upd); err == nil {
		t.Error("retention role UPDATE succeeded, want refused")
	}
	for _, stmt := range []string{del, upd, "INSERT INTO playground_sessions (owner_id) SELECT $1 WHERE false"} {
		err := as("playground_app", stmt)
		if stmt != del && stmt != upd {
			if err != nil {
				t.Errorf("app role %q = %v, want allowed", stmt, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("app role %q succeeded, want refused", stmt)
		}
	}

	// A mistaken grant does not let the app delete: the trigger still refuses.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "GRANT DELETE ON audit_events TO playground_app"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE playground_app"); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, del, s.ID); err == nil {
		t.Error("app role DELETE with a mistaken grant succeeded, want the trigger to refuse")
	}
}

func TestAuditPurgeDeletesOnlyOldEvents(t *testing.T) {
	pool := testPool(t)
	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	purger, err := session.NewPGAuditPurger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s, err := store.Admit(ctx, session.NewSession{
		OwnerID: "owner-purge", ClientKey: "192.0.2.1", Profile: "p", ImageRef: "i",
		Spec: []byte("{}"), MaxDuration: time.Hour, IdleTimeout: time.Minute,
	}, session.Limits{Capacity: 1000, PerOwner: 1000, PerClient: 1000})
	if err != nil {
		t.Fatal(err)
	}

	if n, err := purger.PurgeAuditEvents(ctx, 24*time.Hour, 1000); err != nil {
		t.Fatalf("purge = %d, %v", n, err)
	}
	if es, _ := store.AuditEvents(ctx, s.ID); len(es) != 1 {
		t.Fatalf("a fresh event was purged: %v", es)
	}

	time.Sleep(20 * time.Millisecond)
	for {
		n, err := purger.PurgeAuditEvents(ctx, time.Millisecond, 1000)
		if err != nil {
			t.Fatalf("purge = %v", err)
		}
		if n < 1000 {
			break
		}
	}
	if es, _ := store.AuditEvents(ctx, s.ID); len(es) != 0 {
		t.Fatalf("old events survived: %v", es)
	}
}

// Another replica holding the purge lock makes this one skip, not wait.
func TestPurgeSkipsWhileAnotherReplicaPurges(t *testing.T) {
	pool := testPool(t)
	store, err := session.NewPGStore(pool, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	purger, err := session.NewPGAuditPurger(pool)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, key := range []database.LockKey{database.LockSessionPurge, database.LockAuditPurge} {
		if got, err := database.TryXactLock(ctx, tx, key); err != nil || !got {
			t.Fatalf("setup: lock %v = %v, %v", key, got, err)
		}
	}

	if _, err := store.PurgeDeletedSessions(ctx, time.Hour, 10); !errors.Is(err, session.ErrBusy) {
		t.Errorf("session purge = %v, want ErrBusy", err)
	}
	if _, err := purger.PurgeAuditEvents(ctx, time.Hour, 10); !errors.Is(err, session.ErrBusy) {
		t.Errorf("audit purge = %v, want ErrBusy", err)
	}
}
