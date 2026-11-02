package storetest

import (
	"context"
	"errors"
	"testing"

	"github.com/os-playground-api/internal/session"
)

// auditor is how the contract reads audit events. It is not part of
// session.Store, but every Store under test must implement it.
type auditor interface {
	AuditEvents(ctx context.Context, sessionID string) ([]session.AuditEvent, error)
}

func events(t *testing.T, st session.Store, id string) []session.AuditEvent {
	t.Helper()
	a, ok := st.(auditor)
	if !ok {
		t.Fatalf("%T does not implement AuditEvents", st)
	}
	es, err := a.AuditEvents(context.Background(), id)
	if err != nil {
		t.Fatalf("AuditEvents = %v", err)
	}
	return es
}

func types(es []session.AuditEvent) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Type
	}
	return out
}

func wantTypes(t *testing.T, es []session.AuditEvent, want ...string) {
	t.Helper()
	got := types(es)
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
}

func admitIsAudited(t *testing.T, st session.Store) {
	ctx := session.WithRequestID(context.Background(), "req-1")
	s, err := st.Admit(ctx, newSession(alice, home), roomy)
	if err != nil {
		t.Fatal(err)
	}

	es := events(t, st, s.ID)
	wantTypes(t, es, session.EventCreated)
	e := es[0]
	if e.RequestID != "req-1" || e.UserID != alice || e.SessionID != s.ID || e.OccurredAt.IsZero() {
		t.Errorf("event = %+v, want request req-1, user %s, session %s, a time", e, alice, s.ID)
	}
	if e.Metadata["profile"] != "small" || e.Metadata["image_ref"] != "alpine:3.20" {
		t.Errorf("metadata = %v, want the profile and image", e.Metadata)
	}
	// Privacy (ADR 0010, section 12): audit rows outlive sessions by months,
	// so they never hold the client's network address.
	for k, v := range e.Metadata {
		if v == home {
			t.Errorf("metadata[%q] holds the client address", k)
		}
	}
}

func requestDeleteIsAuditedOnce(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx := session.WithRequestID(context.Background(), "req-del")

	if err := st.RequestDelete(ctx, s.ID, bob); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("RequestDelete as bob = %v", err)
	}
	for range 2 {
		if err := st.RequestDelete(ctx, s.ID, alice); err != nil {
			t.Fatal(err)
		}
	}

	es := events(t, st, s.ID)
	wantTypes(t, es, session.EventCreated, session.EventDeleteRequested)
	if e := es[1]; e.RequestID != "req-del" || e.UserID != alice {
		t.Errorf("event = %+v, want request req-del by %s", e, alice)
	}
}

func statusChangesAreAudited(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Status: session.StatusProvisioning})
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Status: session.StatusProvisioning}) // no change
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{})                                   // no status
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Status: session.StatusFailed, FailureReason: "boom"})

	es := events(t, st, s.ID)
	wantTypes(t, es, session.EventCreated, session.EventStatusChanged, session.EventStatusChanged)
	if m := es[1].Metadata; m["from"] != "CREATED" || m["to"] != "PROVISIONING" {
		t.Errorf("first change = %v", m)
	}
	if m := es[2].Metadata; m["from"] != "PROVISIONING" || m["to"] != "FAILED" || m["reason"] != "boom" {
		t.Errorf("second change = %v", m)
	}
	if es[1].UserID != alice || es[1].RequestID != "" {
		t.Errorf("reconciler event = %+v, want the owner and no request", es[1])
	}
}

func expiryIsAudited(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)
	if err := st.Complete(context.Background(), c, session.Result{ExpireReason: "idle"}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("ExpireReason without DesireDeleted = %v, want ErrInvalid", err)
	}
	mustComplete(t, st, c, session.Result{DesireDeleted: true, ExpireReason: "idle"})

	es := events(t, st, s.ID)
	wantTypes(t, es, session.EventCreated, session.EventExpired)
	if es[1].Metadata["reason"] != "idle" {
		t.Errorf("metadata = %v, want reason idle", es[1].Metadata)
	}
}

func lostLeaseIsNotAudited(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)
	c.Worker = worker2
	if err := st.Complete(context.Background(), c, session.Result{Status: session.StatusRunning}); !errors.Is(err, session.ErrLeaseLost) {
		t.Fatalf("Complete = %v, want ErrLeaseLost", err)
	}
	wantTypes(t, events(t, st, s.ID), session.EventCreated)
}
