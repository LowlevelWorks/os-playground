package storetest

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
)

// SetStatus moves the session id to status the way the reconciler does, by
// claiming and completing it. It is for tests, in any package, that need a
// session in a given state, such as RUNNING before a console can attach.
// Other due sessions are claimed too and completed unchanged.
func SetStatus(t testing.TB, st session.Store, id string, status session.Status) {
	t.Helper()
	ctx := context.Background()
	claims, err := st.Claim(ctx, "storetest-setstatus", time.Minute, 1000)
	if err != nil {
		t.Fatalf("SetStatus: Claim = %v", err)
	}
	found := false
	for _, c := range claims {
		var r session.Result
		if c.ID == id {
			r.Status, found = status, true
		}
		if err := st.Complete(ctx, c, r); err != nil {
			t.Fatalf("SetStatus: Complete = %v", err)
		}
	}
	if !found {
		t.Fatalf("SetStatus: session %s was not due", id)
	}
}

func running(t *testing.T, st session.Store, owner string) session.Session {
	t.Helper()
	s := mustAdmit(t, st, owner, home, roomy)
	SetStatus(t, st, s.ID, session.StatusRunning)
	return s
}

const (
	holder1 = "console-1"
	holder2 = "console-2"
)

func attachNeedsARunningSession(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := mustAdmit(t, st, alice, home, roomy)

	if err := st.AcquireAttach(ctx, s.ID, alice, holder1, lease); !errors.Is(err, session.ErrNotAttachable) {
		t.Errorf("AcquireAttach on CREATED = %v, want ErrNotAttachable", err)
	}
	for _, tc := range []struct{ id, owner string }{{s.ID, bob}, {s.ID, ""}, {missingID, alice}, {"nope", alice}} {
		if err := st.AcquireAttach(ctx, tc.id, tc.owner, holder1, lease); !errors.Is(err, session.ErrNotFound) {
			t.Errorf("AcquireAttach(%q as %q) = %v, want ErrNotFound", tc.id, tc.owner, err)
		}
	}
	if err := st.AcquireAttach(ctx, s.ID, alice, "", lease); !errors.Is(err, session.ErrInvalid) {
		t.Errorf("AcquireAttach with no holder = %v, want ErrInvalid", err)
	}
}

func attachLeaseIsExclusive(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := running(t, st, alice)

	if err := st.AcquireAttach(ctx, s.ID, alice, holder1, lease); err != nil {
		t.Fatalf("AcquireAttach = %v", err)
	}
	if err := st.AcquireAttach(ctx, s.ID, alice, holder2, lease); !errors.Is(err, session.ErrAttached) {
		t.Fatalf("second holder = %v, want ErrAttached", err)
	}
	if err := st.AcquireAttach(ctx, strings.ToUpper(s.ID), alice, holder1, lease); err != nil {
		t.Errorf("the holder acquiring again = %v, want success", err)
	}

	// Releasing someone else's lease does nothing.
	if err := st.ReleaseAttach(ctx, s.ID, holder2); err != nil {
		t.Fatal(err)
	}
	if err := st.AcquireAttach(ctx, s.ID, alice, holder2, lease); !errors.Is(err, session.ErrAttached) {
		t.Fatalf("after a stranger's release = %v, want ErrAttached", err)
	}

	if err := st.ReleaseAttach(ctx, s.ID, holder1); err != nil {
		t.Fatal(err)
	}
	if err := st.ReleaseAttach(ctx, s.ID, holder1); err != nil {
		t.Errorf("second release = %v, want nil", err)
	}
	if err := st.AcquireAttach(ctx, s.ID, alice, holder2, lease); err != nil {
		t.Errorf("after release = %v, want success", err)
	}
}

func expiredAttachLeaseIsTakenOver(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := running(t, st, alice)

	if err := st.AcquireAttach(ctx, s.ID, alice, holder1, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // leases run on the store's clock

	if err := st.AcquireAttach(ctx, s.ID, alice, holder2, lease); err != nil {
		t.Fatalf("after the lease expired = %v, want success", err)
	}
	if err := st.RenewAttach(ctx, s.ID, holder1, lease); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("the old holder's renew = %v, want ErrLeaseLost", err)
	}
}

func renewNeedsTheLease(t *testing.T, st session.Store) {
	ctx := context.Background()
	s := running(t, st, alice)
	if err := st.AcquireAttach(ctx, s.ID, alice, holder1, lease); err != nil {
		t.Fatal(err)
	}

	if err := st.RenewAttach(ctx, s.ID, holder1, lease); err != nil {
		t.Errorf("RenewAttach = %v", err)
	}
	if err := st.RenewAttach(ctx, s.ID, holder2, lease); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("renew by a stranger = %v, want ErrLeaseLost", err)
	}

	// A session marked for deletion keeps no console.
	if err := st.RequestDelete(ctx, s.ID, alice); err != nil {
		t.Fatal(err)
	}
	if err := st.RenewAttach(ctx, s.ID, holder1, lease); !errors.Is(err, session.ErrLeaseLost) {
		t.Errorf("renew after delete = %v, want ErrLeaseLost", err)
	}
	if err := st.AcquireAttach(ctx, s.ID, alice, holder2, lease); !errors.Is(err, session.ErrNotAttachable) {
		t.Errorf("acquire after delete = %v, want ErrNotAttachable", err)
	}
}

// Meant for real PostgreSQL: one conditional UPDATE, one winner.
func concurrentAcquiresHaveOneWinner(t *testing.T, st session.Store) {
	s := running(t, st, alice)

	const n = 20
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		ok, others int
	)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := st.AcquireAttach(context.Background(), s.ID, alice, "console-"+strconv.Itoa(i), lease)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case errors.Is(err, session.ErrAttached):
				others++
			default:
				t.Errorf("AcquireAttach = %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 1 || others != n-1 {
		t.Fatalf("%d won, %d refused; want 1 and %d", ok, others, n-1)
	}
}

func holdingVMListsUnreleasedSessions(t *testing.T, st session.Store) {
	held := mustAdmit(t, st, alice, home, roomy)
	gone := mustAdmit(t, st, bob, office, roomy)
	ctx := context.Background()

	cs := mustClaim(t, st, worker1, 10)
	for _, c := range cs {
		r := session.Result{}
		if c.ID == gone.ID {
			r = session.Result{Status: session.StatusFailed, VMReleased: true}
		}
		mustComplete(t, st, c, r)
	}

	got, err := st.HoldingVM(ctx, []string{held.ID, gone.ID, missingID, "stranger", strings.ToUpper(held.ID)})
	if err != nil {
		t.Fatalf("HoldingVM = %v", err)
	}
	if len(got) != 1 || !got[held.ID] {
		t.Fatalf("HoldingVM = %v, want only %s", got, held.ID)
	}
	if got, err := st.HoldingVM(ctx, nil); err != nil || len(got) != 0 {
		t.Errorf("HoldingVM(nil) = %v, %v; want empty", got, err)
	}
}
