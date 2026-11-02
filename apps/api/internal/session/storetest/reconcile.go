package storetest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
)

// The reconciler's side of the contract: Claim, Complete and TouchActivity.

const (
	worker1 = "worker-1"
	worker2 = "worker-2"
	lease   = time.Minute
)

func mustClaim(t *testing.T, st session.Store, worker string, limit int) []session.Claim {
	t.Helper()
	cs, err := st.Claim(context.Background(), worker, lease, limit)
	if err != nil {
		t.Fatalf("Claim(%s) = %v", worker, err)
	}
	return cs
}

// mustClaimOne claims and fails unless exactly the session id comes back.
func mustClaimOne(t *testing.T, st session.Store, id string) session.Claim {
	t.Helper()
	cs := mustClaim(t, st, worker1, 10)
	if len(cs) != 1 || cs[0].ID != id {
		t.Fatalf("Claim = %d sessions %v, want exactly %s", len(cs), claimIDs(cs), id)
	}
	return cs[0]
}

func mustComplete(t *testing.T, st session.Store, c session.Claim, r session.Result) {
	t.Helper()
	if err := st.Complete(context.Background(), c, r); err != nil {
		t.Fatalf("Complete(%+v) = %v", r, err)
	}
}

func claimIDs(cs []session.Claim) []string {
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	return ids
}

func claimTakesDueSessionsOnce(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)

	c := mustClaimOne(t, st, s.ID)
	if c.Worker != worker1 || c.Status != session.StatusCreated || c.OwnerID != alice ||
		c.VMReleased || !c.TerminatedAt.IsZero() || c.Attempts != 0 {
		t.Errorf("claim = %+v, want worker-1's CREATED session, unreleased, no attempts", c)
	}
	if !sameJSON(t, c.Spec, s.Spec) {
		t.Errorf("claimed Spec = %s, want %s", c.Spec, s.Spec)
	}
	if cs := mustClaim(t, st, worker2, 10); len(cs) != 0 {
		t.Errorf("a second worker claimed %v while the lease is current", claimIDs(cs))
	}
}

func claimHonoursLimitAndOrder(t *testing.T, st session.Store) {
	first := mustAdmit(t, st, alice, home, roomy)
	second := mustAdmit(t, st, bob, home, roomy)
	mustAdmit(t, st, carol, office, roomy)

	cs := mustClaim(t, st, worker1, 2)
	got := map[string]bool{}
	for _, c := range cs {
		got[c.ID] = true
	}
	if len(cs) != 2 || !got[first.ID] || !got[second.ID] {
		t.Fatalf("Claim(limit 2) = %v, want the two due first: %s, %s", claimIDs(cs), first.ID, second.ID)
	}
	if rest := mustClaim(t, st, worker2, 10); len(rest) != 1 {
		t.Fatalf("second Claim = %v, want the one left", claimIDs(rest))
	}
}

func claimRejectsBadArguments(t *testing.T, st session.Store) {
	ctx := context.Background()
	for _, tc := range []struct {
		worker string
		lease  time.Duration
		limit  int
	}{{"", lease, 1}, {worker1, 0, 1}, {worker1, lease, 0}} {
		if _, err := st.Claim(ctx, tc.worker, tc.lease, tc.limit); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("Claim(%q, %v, %d) = %v, want ErrInvalid", tc.worker, tc.lease, tc.limit, err)
		}
	}
}

func completeRecordsTheOutcome(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)

	mustComplete(t, st, c, session.Result{Status: session.StatusRunning})

	got, err := st.Get(context.Background(), s.ID, alice)
	if err != nil || got.Status != session.StatusRunning {
		t.Fatalf("Get = %+v, %v; want RUNNING", got, err)
	}
	// RetryAfter 0: due again at once, and the lease is released.
	if again := mustClaimOne(t, st, s.ID); again.Status != session.StatusRunning {
		t.Errorf("reclaimed status = %s, want RUNNING", again.Status)
	}
}

func completeNeedsTheLease(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)
	ctx := context.Background()

	other := c
	other.Worker = worker2
	if err := st.Complete(ctx, other, session.Result{Status: session.StatusRunning}); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("Complete by another worker = %v, want ErrLeaseLost", err)
	}
	mustComplete(t, st, c, session.Result{Status: session.StatusProvisioning})
	if err := st.Complete(ctx, c, session.Result{Status: session.StatusRunning}); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("second Complete on a released lease = %v, want ErrLeaseLost", err)
	}

	got, _ := st.Get(ctx, s.ID, alice)
	if got.Status != session.StatusProvisioning {
		t.Errorf("status = %s, want PROVISIONING: a lost lease wrote", got.Status)
	}
}

func expiredLeaseCanBeTakenOver(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx := context.Background()
	cs, err := st.Claim(ctx, worker1, time.Millisecond, 10)
	if err != nil || len(cs) != 1 {
		t.Fatalf("Claim = %v, %v", claimIDs(cs), err)
	}
	time.Sleep(20 * time.Millisecond) // the only sleep: leases run on the store's clock

	taken, err := st.Claim(ctx, worker2, lease, 10)
	if err != nil || len(taken) != 1 || taken[0].ID != s.ID {
		t.Fatalf("Claim after the lease expired = %v, %v; want %s", claimIDs(taken), err, s.ID)
	}
	if err := st.Complete(ctx, cs[0], session.Result{Status: session.StatusRunning}); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("Complete by the first worker = %v, want ErrLeaseLost", err)
	}
}

func retryAfterDefersTheSession(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{RetryAfter: time.Hour})

	if cs := mustClaim(t, st, worker1, 10); len(cs) != 0 {
		t.Errorf("Claim = %v, want nothing due for an hour", claimIDs(cs))
	}
}

// A user's delete must not wait out a long RetryAfter.
func requestDeleteMakesTheSessionDue(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Status: session.StatusRunning, RetryAfter: time.Hour})

	if err := st.RequestDelete(context.Background(), s.ID, alice); err != nil {
		t.Fatal(err)
	}
	if c := mustClaimOne(t, st, s.ID); !c.DesiredDeleted {
		t.Errorf("claim = %+v, want DesiredDeleted", c)
	}
}

func failedAttemptsAreCounted(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Failed: true})
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{Failed: true})

	c := mustClaimOne(t, st, s.ID)
	if c.Attempts != 2 {
		t.Fatalf("Attempts = %d, want 2", c.Attempts)
	}
	mustComplete(t, st, c, session.Result{})
	if c := mustClaimOne(t, st, s.ID); c.Attempts != 0 {
		t.Errorf("Attempts after a success = %d, want 0", c.Attempts)
	}
}

// ADR 0010, section 4: a failed session frees capacity as soon as its VM is
// gone, and stays readable with its reason.
func releasingTheVMFreesCapacity(t *testing.T, st session.Store) {
	one := session.Limits{Capacity: 1, PerOwner: 5, PerClient: 5}
	s := mustAdmit(t, st, alice, home, one)
	ctx := context.Background()
	if _, err := st.Admit(ctx, newSession(bob, office), one); !errors.Is(err, session.ErrCapacity) {
		t.Fatalf("setup: Admit = %v, want ErrCapacity", err)
	}

	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{
		Status: session.StatusFailed, FailureReason: "boom", VMReleased: true,
	})

	mustAdmit(t, st, bob, office, one)
	got, err := st.Get(ctx, s.ID, alice)
	if err != nil || got.Status != session.StatusFailed || got.FailureReason != "boom" {
		t.Fatalf("Get = %+v, %v; want FAILED with its reason", got, err)
	}
	c := mustClaim(t, st, worker1, 10)
	for _, c := range c {
		if c.ID == s.ID && (!c.VMReleased || c.TerminatedAt.IsZero()) {
			t.Errorf("claim = %+v, want VMReleased and TerminatedAt set", c)
		}
	}
}

// The schema refuses DELETED while the VM is unreleased; so must every store.
func deletedNeedsAReleasedVM(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)
	ctx := context.Background()

	if err := st.Complete(ctx, c, session.Result{Status: session.StatusDeleted}); !errors.Is(err, session.ErrInvalid) {
		t.Fatalf("Complete(DELETED, VM not released) = %v, want ErrInvalid", err)
	}
	mustComplete(t, st, c, session.Result{Status: session.StatusDeleted, DesireDeleted: true, VMReleased: true})

	if cs := mustClaim(t, st, worker1, 10); len(cs) != 0 {
		t.Errorf("Claim = %v, want a DELETED session never claimed again", claimIDs(cs))
	}
	got, err := st.Get(ctx, s.ID, alice)
	if err != nil || got.Status != session.StatusDeleted {
		t.Errorf("Get = %+v, %v; want the row kept as DELETED", got, err)
	}
}

func completeRejectsAnUnknownStatus(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	c := mustClaimOne(t, st, s.ID)
	for _, r := range []session.Result{{Status: "PAUSED"}, {RetryAfter: -time.Second}} {
		if err := st.Complete(context.Background(), c, r); !errors.Is(err, session.ErrInvalid) {
			t.Errorf("Complete(%+v) = %v, want ErrInvalid", r, err)
		}
	}
}

// Expiry: the reconciler marks the session itself, which frees the quota.
func reconcilerCanDesireDeletion(t *testing.T, st session.Store) {
	one := session.Limits{Capacity: 5, PerOwner: 1, PerClient: 5}
	s := mustAdmit(t, st, alice, home, one)
	mustComplete(t, st, mustClaimOne(t, st, s.ID), session.Result{DesireDeleted: true, RetryAfter: time.Hour})

	got, _ := st.Get(context.Background(), s.ID, alice)
	if !got.DesiredDeleted {
		t.Fatal("DesiredDeleted not set")
	}
	mustAdmit(t, st, alice, home, one)
	if c := mustClaim(t, st, worker1, 10); len(c) == 0 {
		t.Error("a session marked for deletion was not due at once")
	}
}

func touchActivityPushesTheIdleDeadline(t *testing.T, st session.Store) {
	s := mustAdmit(t, st, alice, home, roomy)
	ctx := context.Background()

	if err := st.TouchActivity(ctx, s.ID, 2*time.Hour); err != nil {
		t.Fatalf("TouchActivity = %v", err)
	}
	got, _ := st.Get(ctx, s.ID, alice)
	if !got.IdleExpiresAt.After(s.IdleExpiresAt.Add(time.Hour)) {
		t.Fatalf("IdleExpiresAt = %v, want about 2h from now (was %v)", got.IdleExpiresAt, s.IdleExpiresAt)
	}

	// A shorter touch never pulls the deadline back.
	if err := st.TouchActivity(ctx, s.ID, time.Second); err != nil {
		t.Fatal(err)
	}
	if again, _ := st.Get(ctx, s.ID, alice); !again.IdleExpiresAt.Equal(got.IdleExpiresAt) {
		t.Errorf("IdleExpiresAt = %v, want it kept at %v", again.IdleExpiresAt, got.IdleExpiresAt)
	}

	for _, id := range []string{missingID, "not-a-uuid"} {
		if err := st.TouchActivity(ctx, id, time.Minute); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("TouchActivity(%q) = %v, want ErrNotFound", id, err)
		}
	}
	if err := st.TouchActivity(ctx, s.ID, 0); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("TouchActivity(0) = %v, want ErrInvalid", err)
	}
}

// Meant for -race and real PostgreSQL: SKIP LOCKED must hand each session to
// exactly one of several workers claiming at once.
func concurrentClaimsNeverShare(t *testing.T, st session.Store) {
	const sessions, workers = 20, 8
	for i := range sessions {
		mustAdmit(t, st, "owner-"+strconv.Itoa(i), home, roomy)
	}

	var (
		mu   sync.Mutex
		seen = map[string]int{}
		wg   sync.WaitGroup
	)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				cs, err := st.Claim(context.Background(), "worker-"+strconv.Itoa(w), lease, 3)
				if err != nil {
					t.Errorf("Claim: %v", err)
					return
				}
				if len(cs) == 0 {
					return
				}
				mu.Lock()
				for _, c := range cs {
					seen[c.ID]++
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(seen) != sessions {
		t.Errorf("%d sessions claimed, want %d", len(seen), sessions)
	}
	for id, n := range seen {
		if n != 1 {
			t.Errorf("session %s claimed %d times, want once", id, n)
		}
	}
}
