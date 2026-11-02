package session

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"
)

const (
	defaultPurgeInterval = 24 * time.Hour
	defaultPurgeBatch    = 1000
)

// RetentionConfig configures a Retention job. Sessions and SessionRetention
// are required. Audit may be nil: audit events are then kept, and New logs
// nothing about it; the caller decides how loudly to say so.
type RetentionConfig struct {
	Sessions         Store
	SessionRetention time.Duration

	Audit          AuditPurger
	AuditRetention time.Duration // required when Audit is set

	// LoginSessions, if set, has its expired login sessions deleted too.
	LoginSessions LoginSessionPurger

	Interval time.Duration // how often to purge; zero is daily
	Batch    int           // rows per transaction; zero is 1000
	Logger   *slog.Logger  // nil discards
}

// LoginSessionPurger deletes expired login sessions. *auth.PGStore
// implements it.
type LoginSessionPurger interface {
	PurgeExpired(ctx context.Context, limit int) (int, error)
}

// Purged counts what one PurgeOnce removed.
type Purged struct {
	Sessions      int
	AuditEvents   int
	LoginSessions int
}

func (p Purged) any() bool { return p.Sessions > 0 || p.AuditEvents > 0 || p.LoginSessions > 0 }

// Retention deletes DELETED sessions and audit events once they are older
// than their retention periods (ADR 0010, section 12). It works in small
// batches, each its own short transaction, so it never holds a lock for
// long; every replica runs it, and a try-lock lets one at a time do work.
type Retention struct {
	sessions    Store
	sessionKeep time.Duration
	audit       AuditPurger
	auditKeep   time.Duration
	logins      LoginSessionPurger
	interval    time.Duration
	batch       int
	log         *slog.Logger
}

// NewRetention checks the configuration.
func NewRetention(cfg RetentionConfig) (*Retention, error) {
	var errs []error
	if cfg.Sessions == nil {
		errs = append(errs, errors.New("session: retention Sessions is required"))
	}
	if cfg.SessionRetention <= 0 {
		errs = append(errs, fmt.Errorf("session: SessionRetention=%s: must be greater than 0", cfg.SessionRetention))
	}
	if cfg.Audit != nil && cfg.AuditRetention <= 0 {
		errs = append(errs, fmt.Errorf("session: AuditRetention=%s: must be greater than 0", cfg.AuditRetention))
	}
	if cfg.Interval < 0 || cfg.Batch < 0 {
		errs = append(errs, fmt.Errorf("session: Interval=%s, Batch=%d: must not be negative", cfg.Interval, cfg.Batch))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Retention{
		sessions:    cfg.Sessions,
		sessionKeep: cfg.SessionRetention,
		audit:       cfg.Audit,
		auditKeep:   cfg.AuditRetention,
		logins:      cfg.LoginSessions,
		interval:    cmp.Or(cfg.Interval, defaultPurgeInterval),
		batch:       cmp.Or(cfg.Batch, defaultPurgeBatch),
		log:         cfg.Logger,
	}, nil
}

// Run purges once at start and then every Interval, until ctx ends.
func (r *Retention) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	for {
		p, err := r.PurgeOnce(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			r.log.Error("retention purge failed", "sessions", p.Sessions, "audit_events", p.AuditEvents,
				"login_sessions", p.LoginSessions, "err", err)
		case p.any():
			r.log.Info("retention purge", "sessions", p.Sessions, "audit_events", p.AuditEvents,
				"login_sessions", p.LoginSessions)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PurgeOnce runs every purge to completion and returns how many rows each
// removed. A purge another replica is running is skipped, not an error.
func (r *Retention) PurgeOnce(ctx context.Context) (Purged, error) {
	var p Purged
	var errs []error
	n, err := r.drain(ctx, func(ctx context.Context) (int, error) {
		return r.sessions.PurgeDeletedSessions(ctx, r.sessionKeep, r.batch)
	})
	p.Sessions, errs = n, append(errs, err)
	if r.audit != nil {
		n, err := r.drain(ctx, func(ctx context.Context) (int, error) {
			return r.audit.PurgeAuditEvents(ctx, r.auditKeep, r.batch)
		})
		p.AuditEvents, errs = n, append(errs, err)
	}
	if r.logins != nil {
		// Expired login sessions authenticate nothing, so any replica may
		// delete them at any time; no lock is needed.
		n, err := r.drain(ctx, func(ctx context.Context) (int, error) {
			return r.logins.PurgeExpired(ctx, r.batch)
		})
		p.LoginSessions, errs = n, append(errs, err)
	}
	return p, errors.Join(errs...)
}

// drain calls purge until a batch comes back short.
func (r *Retention) drain(ctx context.Context, purge func(context.Context) (int, error)) (int, error) {
	total := 0
	for {
		n, err := purge(ctx)
		total += n
		switch {
		case errors.Is(err, ErrBusy):
			r.log.Debug("retention purge skipped: another replica is purging")
			return total, nil
		case err != nil:
			return total, err
		case n < r.batch:
			return total, nil
		}
		if err := ctx.Err(); err != nil {
			return total, err
		}
	}
}
