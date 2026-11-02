package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/os-playground-api/internal/database"
)

// PGStore is the PostgreSQL Store, the source of truth in production.
//
// Only Admit takes the admission lock. RequestDelete and the reconciler only
// ever lower the counts Admit reads, so an Admit that runs alongside them can
// at worst refuse a request it could have accepted, never admit too many.
type PGStore struct {
	pool     *pgxpool.Pool
	lockWait time.Duration
}

var _ Store = (*PGStore)(nil)

// NewPGStore returns a store on pool. lockWait bounds how long Admit queues
// for the admission lock before returning ErrBusy; it must be positive,
// because PostgreSQL reads a lock_timeout of 0 as "wait forever".
func NewPGStore(pool *pgxpool.Pool, lockWait time.Duration) (*PGStore, error) {
	if pool == nil {
		return nil, errors.New("session: PGStore needs a pool")
	}
	if lockWait <= 0 {
		return nil, fmt.Errorf("session: lockWait=%s: must be greater than 0", lockWait)
	}
	return &PGStore{pool: pool, lockWait: lockWait}, nil
}

// sessionColumns is what every query returning a Session selects, in the
// order scanSession reads them.
const sessionColumns = `id::text, owner_id, client_key, profile, image_ref, spec, status,
	desired = 'DELETED', coalesce(failure_reason, ''), created_at, expires_at, idle_expires_at`

func scanSession(row pgx.Row) (Session, error) {
	var s Session
	err := row.Scan(&s.ID, &s.OwnerID, &s.ClientKey, &s.Profile, &s.ImageRef, &s.Spec,
		&s.Status, &s.DesiredDeleted, &s.FailureReason, &s.CreatedAt, &s.ExpiresAt, &s.IdleExpiresAt)
	return s, err
}

// The three queries Admit counts with. Each WHERE clause repeats its partial
// index's predicate exactly, or PostgreSQL scans the whole table (see the
// init and add_desired migrations).

// liveOwnerIDs uses sessions_live_by_owner. Oldest first, for QuotaError.ID.
func liveOwnerIDs(ctx context.Context, tx pgx.Tx, ownerID string) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT id::text FROM playground_sessions
		WHERE owner_id = $1 AND desired = 'RUNNING'
		  AND status IN ('CREATED', 'PROVISIONING', 'STARTING', 'RUNNING')
		ORDER BY created_at, id`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("session: live sessions by owner: %w", err)
	}
	ids, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("session: live sessions by owner: %w", err)
	}
	return ids, nil
}

// countHolding uses sessions_holding_vm.
func countHolding(ctx context.Context, tx pgx.Tx) (int, error) {
	var n int
	err := tx.QueryRow(ctx,
		`SELECT count(*) FROM playground_sessions WHERE vm_released_at IS NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("session: count sessions holding a VM: %w", err)
	}
	return n, nil
}

// countClientLive uses sessions_live_by_client.
func countClientLive(ctx context.Context, tx pgx.Tx, clientKey string) (int, error) {
	var n int
	err := tx.QueryRow(ctx, `SELECT count(*) FROM playground_sessions
		WHERE client_key = $1 AND desired = 'RUNNING'
		  AND status IN ('CREATED', 'PROVISIONING', 'STARTING', 'RUNNING')`, clientKey).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("session: count live sessions by client: %w", err)
	}
	return n, nil
}

// insertSession computes both expiry times from the same now() as
// created_at, so ExpiresAt - CreatedAt is exactly MaxDuration.
func insertSession(ctx context.Context, tx pgx.Tx, n NewSession) (Session, error) {
	s, err := scanSession(tx.QueryRow(ctx, `INSERT INTO playground_sessions
			(owner_id, client_key, profile, spec, image_ref, expires_at, idle_expires_at)
		VALUES ($1, $2, $3, $4, $5, now() + $6::interval, now() + $7::interval)
		RETURNING `+sessionColumns,
		n.OwnerID, n.ClientKey, n.Profile, n.Spec, n.ImageRef, n.MaxDuration, n.IdleTimeout))
	if err != nil {
		return Session{}, fmt.Errorf("session: insert: %w", err)
	}
	return s, nil
}

// Admit implements Store.Admit under the admission advisory lock. Input is
// checked before the lock is taken, so a bad request costs no connection.
// ErrCapacity and *QuotaError come back unwrapped: pgx.BeginFunc returns the
// callback's error as is.
func (p *PGStore) Admit(ctx context.Context, n NewSession, l Limits) (Session, error) {
	if err := checkAdmit(ctx, n, l); err != nil {
		return Session{}, err
	}

	var out Session
	err := database.InLockedTx(ctx, p.pool, database.LockAdmission, p.lockWait, func(tx pgx.Tx) error {
		ownerIDs, err := liveOwnerIDs(ctx, tx, n.OwnerID)
		if err != nil {
			return err
		}
		if len(ownerIDs) >= l.PerOwner {
			return &QuotaError{Yours: true, ID: ownerIDs[0]}
		}

		holding, err := countHolding(ctx, tx)
		if err != nil {
			return err
		}
		if holding >= l.Capacity {
			return ErrCapacity
		}

		clientLive, err := countClientLive(ctx, tx, n.ClientKey)
		if err != nil {
			return err
		}
		if clientLive >= l.PerClient {
			q := &QuotaError{ClientWide: true}
			if len(ownerIDs) > 0 {
				q.Yours, q.ID = true, ownerIDs[0]
			}
			return q
		}

		out, err = insertSession(ctx, tx, n)
		if err != nil {
			return err
		}
		return insertAudit(ctx, tx, createdEvent(ctx, out))
	})
	switch {
	case err == nil:
		return out, nil
	case database.IsLockTimeout(err):
		return Session{}, ErrBusy
	default:
		return Session{}, err
	}
}

// Get implements Store.Get. A malformed ID is ErrNotFound, and the query
// uses the canonical form, which PostgreSQL always accepts.
func (p *PGStore) Get(ctx context.Context, id, ownerID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}
	cid, ok := canonicalID(id)
	if !ok || ownerID == "" {
		return Session{}, ErrNotFound
	}

	s, err := scanSession(p.pool.QueryRow(ctx, `SELECT `+sessionColumns+`
		FROM playground_sessions
		WHERE id = $1 AND owner_id = $2`, cid, ownerID))
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return Session{}, ErrNotFound
	case err != nil:
		return Session{}, fmt.Errorf("session: get: %w", err)
	}
	return s, nil
}

// RequestDelete implements Store.RequestDelete. The row is locked first, so
// two concurrent calls record one event between them; a repeat changes
// nothing and records nothing.
func (p *PGStore) RequestDelete(ctx context.Context, id, ownerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cid, ok := canonicalID(id)
	if !ok || ownerID == "" {
		return ErrNotFound
	}

	return database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		var deleted bool
		err := tx.QueryRow(ctx, `SELECT desired = 'DELETED' FROM playground_sessions
			WHERE id = $1 AND owner_id = $2
			FOR UPDATE`, cid, ownerID).Scan(&deleted)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNotFound
		case err != nil:
			return fmt.Errorf("session: request delete: %w", err)
		case deleted:
			return nil
		}

		if _, err := tx.Exec(ctx, `UPDATE playground_sessions
			SET desired = 'DELETED', next_attempt_at = now()
			WHERE id = $1`, cid); err != nil {
			return fmt.Errorf("session: request delete: %w", err)
		}
		return insertAudit(ctx, tx, deleteRequestedEvent(ctx, cid, ownerID))
	})
}

// Claim implements Store.Claim with FOR UPDATE SKIP LOCKED, so workers
// running at once take different sessions. Its WHERE clause matches the
// sessions_not_deleted index.
func (p *PGStore) Claim(ctx context.Context, worker string, lease time.Duration, limit int) ([]Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if worker == "" || lease <= 0 || limit < 1 {
		return nil, ErrInvalid
	}

	rows, err := p.pool.Query(ctx, `UPDATE playground_sessions
		SET claimed_by = $1, claim_until = now() + $2::interval
		WHERE id IN (
			SELECT id FROM playground_sessions
			WHERE status <> 'DELETED' AND next_attempt_at <= now()
			  AND (claim_until IS NULL OR claim_until < now())
			ORDER BY next_attempt_at, id
			LIMIT $3
			FOR UPDATE SKIP LOCKED)
		RETURNING `+sessionColumns+`, vm_released_at IS NOT NULL, terminated_at, attempts`,
		worker, lease, limit)
	if err != nil {
		return nil, fmt.Errorf("session: claim: %w", err)
	}
	claims, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Claim, error) {
		var (
			c          Claim
			terminated *time.Time
		)
		err := row.Scan(&c.ID, &c.OwnerID, &c.ClientKey, &c.Profile, &c.ImageRef, &c.Spec,
			&c.Status, &c.DesiredDeleted, &c.FailureReason, &c.CreatedAt, &c.ExpiresAt, &c.IdleExpiresAt,
			&c.VMReleased, &terminated, &c.Attempts)
		if terminated != nil {
			c.TerminatedAt = *terminated
		}
		c.Worker = worker
		return c, err
	})
	if err != nil {
		return nil, fmt.Errorf("session: claim: %w", err)
	}
	return claims, nil
}

// Complete implements Store.Complete. The WHERE on claimed_by is the lease
// check: zero rows means another worker took the session over.
func (p *PGStore) Complete(ctx context.Context, c Claim, r Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.validate(c); err != nil {
		return err
	}
	if c.Worker == "" {
		return ErrLeaseLost
	}

	return database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		var (
			old        Status
			oldDeleted bool
			ownerID    string
		)
		err := tx.QueryRow(ctx, `SELECT status, desired = 'DELETED', owner_id FROM playground_sessions
			WHERE id = $1 AND claimed_by = $2
			FOR UPDATE`, c.ID, c.Worker).Scan(&old, &oldDeleted, &ownerID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrLeaseLost
		case err != nil:
			return fmt.Errorf("session: complete: %w", err)
		}

		if _, err := tx.Exec(ctx, `UPDATE playground_sessions SET
			status          = coalesce(nullif($3::text, ''), status),
			failure_reason  = coalesce(nullif($4::text, ''), failure_reason),
			desired         = CASE WHEN $5::boolean THEN 'DELETED' ELSE desired END,
			vm_released_at  = CASE WHEN $6::boolean THEN coalesce(vm_released_at, now()) ELSE vm_released_at END,
			started_at      = CASE WHEN $3 = 'RUNNING' THEN coalesce(started_at, now()) ELSE started_at END,
			terminated_at   = CASE WHEN $3 IN ('STOPPED', 'FAILED', 'DELETED')
			                       THEN coalesce(terminated_at, now()) ELSE terminated_at END,
			attempts        = CASE WHEN $7::boolean THEN attempts + 1 ELSE 0 END,
			next_attempt_at = CASE WHEN $5 OR desired = 'DELETED' THEN now()
			                       ELSE now() + $8::interval END,
			claimed_by      = NULL,
			claim_until     = NULL
		WHERE id = $1 AND claimed_by = $2`,
			c.ID, c.Worker, string(r.Status), r.FailureReason, r.DesireDeleted, r.VMReleased, r.Failed, r.RetryAfter); err != nil {
			return fmt.Errorf("session: complete: %w", err)
		}
		for _, e := range completeEvents(c.ID, ownerID, old, oldDeleted, r) {
			if err := insertAudit(ctx, tx, e); err != nil {
				return err
			}
		}
		return nil
	})
}

// TouchActivity implements Store.TouchActivity. greatest() keeps a later
// deadline, so a slow touch never pulls it back.
func (p *PGStore) TouchActivity(ctx context.Context, id string, idleFor time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if idleFor <= 0 {
		return ErrInvalid
	}
	cid, ok := canonicalID(id)
	if !ok {
		return ErrNotFound
	}

	tag, err := p.pool.Exec(ctx, `UPDATE playground_sessions
		SET last_activity_at = now(),
		    idle_expires_at = greatest(idle_expires_at, now() + $2::interval)
		WHERE id = $1`, cid, idleFor)
	if err != nil {
		return fmt.Errorf("session: touch activity: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// insertAudit writes one audit event in tx, so it commits or rolls back with
// the change it records.
func insertAudit(ctx context.Context, tx pgx.Tx, e AuditEvent) error {
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events
			(event_type, request_id, user_id, session_id, metadata)
		VALUES ($1, nullif($2, ''), $3, $4, $5)`,
		e.Type, e.RequestID, e.UserID, e.SessionID, metadataJSON(e.Metadata)); err != nil {
		return fmt.Errorf("session: audit %s: %w", e.Type, err)
	}
	return nil
}

// AuditEvents returns the events recorded for a session, oldest first. It
// is for tests and operators and is not part of Store.
func (p *PGStore) AuditEvents(ctx context.Context, sessionID string) ([]AuditEvent, error) {
	rows, err := p.pool.Query(ctx, `SELECT occurred_at, event_type, coalesce(request_id, ''),
			coalesce(user_id, ''), coalesce(session_id, ''), metadata
		FROM audit_events
		WHERE session_id = $1
		ORDER BY id`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("session: audit events: %w", err)
	}
	events, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (AuditEvent, error) {
		var e AuditEvent
		err := row.Scan(&e.OccurredAt, &e.Type, &e.RequestID, &e.UserID, &e.SessionID, &e.Metadata)
		return e, err
	})
	if err != nil {
		return nil, fmt.Errorf("session: audit events: %w", err)
	}
	return events, nil
}

// HoldingVM implements Store.HoldingVM. It matches the sessions_holding_vm
// index. IDs that are not canonical UUIDs cannot be session IDs, so they are
// dropped before the query rather than failing the cast.
func (p *PGStore) HoldingVM(ctx context.Context, ids []string) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	valid := make([]string, 0, len(ids))
	for _, id := range ids {
		if cid, ok := canonicalID(id); ok && cid == id {
			valid = append(valid, id)
		}
	}
	out := make(map[string]bool)
	if len(valid) == 0 {
		return out, nil
	}

	rows, err := p.pool.Query(ctx, `SELECT id::text FROM playground_sessions
		WHERE id = ANY($1::uuid[]) AND vm_released_at IS NULL`, valid)
	if err != nil {
		return nil, fmt.Errorf("session: holding vm: %w", err)
	}
	held, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("session: holding vm: %w", err)
	}
	for _, id := range held {
		out[id] = true
	}
	return out, nil
}

// AcquireAttach implements Store.AcquireAttach with one conditional UPDATE,
// so two consoles racing for the lease cannot both win. Only when it matches
// nothing does a second query say why.
func (p *PGStore) AcquireAttach(ctx context.Context, id, ownerID, holder string, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if holder == "" || lease <= 0 {
		return ErrInvalid
	}
	cid, ok := canonicalID(id)
	if !ok || ownerID == "" {
		return ErrNotFound
	}

	tag, err := p.pool.Exec(ctx, `UPDATE playground_sessions
		SET attach_holder = $3, attach_lease_until = now() + $4::interval
		WHERE id = $1 AND owner_id = $2 AND desired = 'RUNNING' AND status = 'RUNNING'
		  AND (attach_holder IS NULL OR attach_holder = $3 OR attach_lease_until < now())`,
		cid, ownerID, holder, lease)
	if err != nil {
		return fmt.Errorf("session: acquire attach: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	var notAttachable bool
	err = p.pool.QueryRow(ctx, `SELECT desired = 'DELETED' OR status <> 'RUNNING'
		FROM playground_sessions
		WHERE id = $1 AND owner_id = $2`, cid, ownerID).Scan(&notAttachable)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return ErrNotFound
	case err != nil:
		return fmt.Errorf("session: acquire attach: %w", err)
	case notAttachable:
		return ErrNotAttachable
	}
	return ErrAttached
}

// RenewAttach implements Store.RenewAttach.
func (p *PGStore) RenewAttach(ctx context.Context, id, holder string, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if holder == "" || lease <= 0 {
		return ErrInvalid
	}
	cid, ok := canonicalID(id)
	if !ok {
		return ErrLeaseLost
	}

	tag, err := p.pool.Exec(ctx, `UPDATE playground_sessions
		SET attach_lease_until = now() + $3::interval
		WHERE id = $1 AND attach_holder = $2 AND desired = 'RUNNING'`, cid, holder, lease)
	if err != nil {
		return fmt.Errorf("session: renew attach: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}

// ReleaseAttach implements Store.ReleaseAttach.
func (p *PGStore) ReleaseAttach(ctx context.Context, id, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cid, ok := canonicalID(id)
	if !ok || holder == "" {
		return nil
	}

	if _, err := p.pool.Exec(ctx, `UPDATE playground_sessions
		SET attach_holder = NULL, attach_lease_until = NULL
		WHERE id = $1 AND attach_holder = $2`, cid, holder); err != nil {
		return fmt.Errorf("session: release attach: %w", err)
	}
	return nil
}
