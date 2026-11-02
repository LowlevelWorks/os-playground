package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/os-playground-api/internal/database"
)

// PGStore is the PostgreSQL Store.
type PGStore struct {
	pool *pgxpool.Pool
}

var _ Store = (*PGStore)(nil)

// NewPGStore returns a store on pool.
func NewPGStore(pool *pgxpool.Pool) (*PGStore, error) {
	if pool == nil {
		return nil, errors.New("auth: PGStore needs a pool")
	}
	return &PGStore{pool: pool}, nil
}

// Login implements Store.Login.
func (p *PGStore) Login(ctx context.Context, id Identity, ttl time.Duration, requestID string) (User, string, error) {
	if !id.valid() || ttl <= 0 {
		return User{}, "", ErrInvalid
	}
	token := newToken()
	hash, _ := tokenHash(token)

	var u User
	err := database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		var disabled bool
		err := tx.QueryRow(ctx, `INSERT INTO users (issuer, subject, email, display_name)
			VALUES ($1, $2, nullif($3, ''), nullif($4, ''))
			ON CONFLICT (issuer, subject) DO UPDATE
				SET email = EXCLUDED.email, display_name = EXCLUDED.display_name, updated_at = now()
			RETURNING id::text, coalesce(email, ''), coalesce(display_name, ''), disabled_at IS NOT NULL`,
			id.Issuer, id.Subject, id.Email, id.Name).Scan(&u.ID, &u.Email, &u.Name, &disabled)
		if err != nil {
			return fmt.Errorf("auth: upsert user: %w", err)
		}
		if disabled {
			return ErrDisabled
		}
		if _, err := tx.Exec(ctx, `INSERT INTO login_sessions (token_hash, user_id, expires_at)
			VALUES ($1, $2, now() + $3::interval)`, hash, u.ID, ttl); err != nil {
			return fmt.Errorf("auth: create login session: %w", err)
		}
		return audit(ctx, tx, EventLoggedIn, requestID, u.ID)
	})
	if err != nil {
		return User{}, "", err
	}
	return u, token, nil
}

// Lookup implements Store.Lookup. It runs on every authenticated request:
// one primary-key lookup and a join on users' primary key.
func (p *PGStore) Lookup(ctx context.Context, token string) (User, error) {
	hash, ok := tokenHash(token)
	if !ok {
		return User{}, ErrNoSession
	}
	var u User
	err := p.pool.QueryRow(ctx, `SELECT u.id::text, coalesce(u.email, ''), coalesce(u.display_name, '')
		FROM login_sessions s
		JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now() AND u.disabled_at IS NULL`, hash).
		Scan(&u.ID, &u.Email, &u.Name)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return User{}, ErrNoSession
	case err != nil:
		return User{}, fmt.Errorf("auth: lookup: %w", err)
	}
	return u, nil
}

// Logout implements Store.Logout.
func (p *PGStore) Logout(ctx context.Context, token, requestID string) error {
	hash, ok := tokenHash(token)
	if !ok {
		return ErrNoSession
	}
	return database.InTx(ctx, p.pool, func(tx pgx.Tx) error {
		var userID string
		err := tx.QueryRow(ctx, `DELETE FROM login_sessions WHERE token_hash = $1
			RETURNING user_id::text`, hash).Scan(&userID)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			return ErrNoSession
		case err != nil:
			return fmt.Errorf("auth: logout: %w", err)
		}
		return audit(ctx, tx, EventLoggedOut, requestID, userID)
	})
}

// PurgeExpired implements Store.PurgeExpired.
func (p *PGStore) PurgeExpired(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, ErrInvalid
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM login_sessions
		WHERE token_hash IN (
			SELECT token_hash FROM login_sessions
			WHERE expires_at <= now()
			ORDER BY expires_at
			LIMIT $1)`, limit)
	if err != nil {
		return 0, fmt.Errorf("auth: purge login sessions: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

func audit(ctx context.Context, tx pgx.Tx, eventType, requestID, userID string) error {
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events (event_type, request_id, user_id)
		VALUES ($1, nullif($2, ''), $3)`, eventType, requestID, userID); err != nil {
		return fmt.Errorf("auth: audit %s: %w", eventType, err)
	}
	return nil
}
