package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const (
	keepSessions = 30 * 24 * time.Hour
	keepAudit    = 90 * 24 * time.Hour
)

func newRetention(t *testing.T, st Store, audit AuditPurger) *Retention {
	t.Helper()
	r, err := NewRetention(RetentionConfig{
		Sessions: st, SessionRetention: keepSessions,
		Audit: audit, AuditRetention: keepAudit,
		Batch: 2, // small, so a purge takes several batches
	})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestNewRetentionChecksItsConfig(t *testing.T) {
	_, err := NewRetention(RetentionConfig{Audit: NewMemStore(time.Now), Batch: -1})
	if err == nil {
		t.Fatal("NewRetention(bad) = nil error")
	}
	for _, want := range []string{"Sessions", "SessionRetention", "AuditRetention", "Batch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// deletedSessions makes n sessions and drives them to DELETED through the
// manager and the reconciler, as in production.
func (r *recRig) deletedSessions(n int) []string {
	r.t.Helper()
	ids := make([]string, n)
	for i := range n {
		o := Owner{UserID: "owner" + string(rune('a'+i)), Client: carol.Client}
		ids[i] = r.create(o)
		if err := r.m.Delete(context.Background(), ids[i], o); err != nil {
			r.t.Fatal(err)
		}
	}
	r.pass(0)
	for _, id := range ids {
		r.wantStatus(id, StatusDeleted)
	}
	return ids
}

func TestRetentionPurgesEachKindAfterItsPeriod(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	ids := r.deletedSessions(5)
	ret := newRetention(t, r.store, r.store)
	ctx := context.Background()

	r.clk.Advance(keepSessions - time.Hour)
	if p, err := ret.PurgeOnce(ctx); err != nil || p.any() {
		t.Fatalf("before 30 days: %+v, %v; want nothing", p, err)
	}

	r.clk.Advance(2 * time.Hour)
	p, err := ret.PurgeOnce(ctx)
	if err != nil || p.Sessions != 5 || p.AuditEvents != 0 {
		t.Fatalf("after 30 days: %+v, %v; want all 5 sessions, no events", p, err)
	}
	if es, _ := r.store.AuditEvents(ctx, ids[0]); len(es) == 0 {
		t.Fatal("audit history went with the session row")
	}

	r.clk.Advance(keepAudit - keepSessions)
	if p, err := ret.PurgeOnce(ctx); err != nil || p.AuditEvents == 0 {
		t.Fatalf("after 90 days: %+v, %v; want events purged", p, err)
	}
	for _, id := range ids {
		if es, _ := r.store.AuditEvents(ctx, id); len(es) != 0 {
			t.Fatalf("events for %s survived 90 days: %v", id, es)
		}
	}
}

func TestRetentionWithoutAnAuditPurgerKeepsEvents(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	ids := r.deletedSessions(1)
	ret := newRetention(t, r.store, nil)

	r.clk.Advance(365 * 24 * time.Hour)
	if p, err := ret.PurgeOnce(context.Background()); err != nil || p.Sessions != 1 || p.AuditEvents != 0 {
		t.Fatalf("PurgeOnce = %+v, %v; want the session only", p, err)
	}
	if es, _ := r.store.AuditEvents(context.Background(), ids[0]); len(es) == 0 {
		t.Fatal("events were purged without an audit purger")
	}
}

// busyStore is a store whose purge lock another replica holds.
type busyStore struct{ Store }

func (busyStore) PurgeDeletedSessions(context.Context, time.Duration, int) (int, error) {
	return 0, ErrBusy
}

type brokenPurger struct{}

func (brokenPurger) PurgeAuditEvents(context.Context, time.Duration, int) (int, error) {
	return 0, errors.New("permission denied to set role")
}

func TestRetentionSkipsABusyPurgeAndReportsAFailedOne(t *testing.T) {
	ret := newRetention(t, busyStore{}, brokenPurger{})
	_, err := ret.PurgeOnce(context.Background())
	if err == nil || !strings.Contains(err.Error(), "set role") || errors.Is(err, ErrBusy) {
		t.Fatalf("PurgeOnce = %v, want only the audit purger's error", err)
	}
}

type countingLogins struct{ left int }

func (c *countingLogins) PurgeExpired(_ context.Context, limit int) (int, error) {
	n := min(c.left, limit)
	c.left -= n
	return n, nil
}

func TestRetentionPurgesExpiredLoginSessions(t *testing.T) {
	logins := &countingLogins{left: 5}
	ret, err := NewRetention(RetentionConfig{
		Sessions: NewMemStore(time.Now), SessionRetention: keepSessions, LoginSessions: logins, Batch: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if p, err := ret.PurgeOnce(context.Background()); err != nil || p.LoginSessions != 5 {
		t.Fatalf("PurgeOnce = %+v, %v; want 5 login sessions over several batches", p, err)
	}
}
