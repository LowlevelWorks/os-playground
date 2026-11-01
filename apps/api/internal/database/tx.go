package database

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type LockKey struct{ Namespace, ID int32 }

const nsSession int32 = 1

// LockAdmission serialises session admission (ADR 0010, section 5).
var LockAdmission = LockKey{nsSession, 1}

// InTx runs fn in one transaction: commit on nil, rollback on error or panic.
func InTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, fn)
}

// InLockedTx is InTx holding a transaction-scoped advisory lock. Postgres
// releases the lock at commit or rollback, so a crashed request can't leak it.
// wait limits how long a request queues for the lock (holding a connection).
func InLockedTx(ctx context.Context, pool *pgxpool.Pool, key LockKey, wait time.Duration, fn func(pgx.Tx) error) error {
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, "SELECT set_config('lock_timeout', $1, true)",
			fmt.Sprintf("%dms", wait.Milliseconds())); err != nil {
			return fmt.Errorf("set lock_timeout: %w", err)
		}

		if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock($1, $2)", key.Namespace, key.ID); err != nil {
			return fmt.Errorf("acquire advisory lock %v: %w", key, err)
		}
		return fn(tx)

	})
}

// IsLockTimeout reports a lock_timeout expiry (SQLSTATE 55P03).
func IsLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "55P03"
}
