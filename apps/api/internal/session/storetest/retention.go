package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
)

// Only DELETED rows past the retention period go. A FAILED session whose VM
// is released stays: it is still readable, and not yet DELETED.
func purgeRemovesOnlyOldDeletedSessions(t *testing.T, st session.Store) {
	ctx := context.Background()
	gone := mustAdmit(t, st, alice, home, roomy)
	failed := mustAdmit(t, st, bob, home, roomy)
	live := mustAdmit(t, st, carol, office, roomy)

	for _, c := range mustClaim(t, st, worker1, 10) {
		var r session.Result
		switch c.ID {
		case gone.ID:
			r = session.Result{Status: session.StatusDeleted, DesireDeleted: true, VMReleased: true}
		case failed.ID:
			r = session.Result{Status: session.StatusFailed, VMReleased: true}
		}
		mustComplete(t, st, c, r)
	}

	if n, err := st.PurgeDeletedSessions(ctx, time.Hour, 10); err != nil || n != 0 {
		t.Fatalf("purge with 1h retention = %d, %v; want nothing old enough", n, err)
	}
	time.Sleep(20 * time.Millisecond) // retention runs on the store's clock

	if n, err := st.PurgeDeletedSessions(ctx, time.Millisecond, 10); err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want 1", n, err)
	}
	if _, err := st.Get(ctx, gone.ID, alice); !errors.Is(err, session.ErrNotFound) {
		t.Errorf("purged session: Get = %v, want ErrNotFound", err)
	}
	for _, s := range []session.Session{failed, live} {
		if _, err := st.Get(ctx, s.ID, s.OwnerID); err != nil {
			t.Errorf("session %s (%s) was purged: %v", s.ID, s.OwnerID, err)
		}
	}
	// Audit history outlives the row: no foreign key ties them.
	wantTypes(t, events(t, st, gone.ID), session.EventCreated, session.EventStatusChanged)

	if n, err := st.PurgeDeletedSessions(ctx, time.Millisecond, 10); err != nil || n != 0 {
		t.Errorf("second purge = %d, %v; want 0", n, err)
	}
	for _, tc := range []struct {
		older time.Duration
		limit int
	}{{0, 10}, {time.Hour, 0}} {
		if _, err := st.PurgeDeletedSessions(ctx, tc.older, tc.limit); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("PurgeDeletedSessions(%v, %d) = %v, want ErrInvalid", tc.older, tc.limit, err)
		}
	}
}

func purgeHonoursTheLimit(t *testing.T, st session.Store) {
	ctx := context.Background()
	for _, owner := range []string{alice, bob, carol} {
		mustAdmit(t, st, owner, home, roomy)
	}
	for _, c := range mustClaim(t, st, worker1, 10) {
		mustComplete(t, st, c, session.Result{Status: session.StatusDeleted, DesireDeleted: true, VMReleased: true})
	}
	time.Sleep(20 * time.Millisecond)

	if n, err := st.PurgeDeletedSessions(ctx, time.Millisecond, 2); err != nil || n != 2 {
		t.Fatalf("purge(limit 2) = %d, %v; want 2", n, err)
	}
	if n, err := st.PurgeDeletedSessions(ctx, time.Millisecond, 2); err != nil || n != 1 {
		t.Fatalf("next purge = %d, %v; want the 1 left", n, err)
	}
}
