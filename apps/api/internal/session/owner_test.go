package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// quotaError fails the test unless err is a *QuotaError.
func quotaError(t *testing.T, err error) *QuotaError {
	t.Helper()
	var q *QuotaError
	if !errors.As(err, &q) || !errors.Is(err, ErrQuota) {
		t.Fatalf("err = %v, want a *QuotaError matching ErrQuota", err)
	}
	return q
}

func TestAnotherOwnerCannotGetAttachOrDelete(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)
	r.boot(id)
	ctx := context.Background()

	for _, o := range []Owner{bob, carol, {Client: alice.Client}} {
		if _, err := r.m.Get(ctx, id, o); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Get as %q = %v, want ErrNotFound", o.Hash, err)
		}
		if _, err := r.m.Console(ctx, id, o); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Console as %q = %v, want ErrNotFound", o.Hash, err)
		}
		if err := r.m.Delete(ctx, id, o); !errors.Is(err, vm.ErrNotFound) {
			t.Errorf("Delete as %q = %v, want ErrNotFound", o.Hash, err)
		}
	}

	if _, err := r.m.Get(ctx, id, alice); err != nil {
		t.Fatalf("the owner lost access: %v", err)
	}
}

func TestTheOwnerIsTheTokenNotTheAddress(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)

	moved := Owner{Hash: alice.Hash, Client: "192.0.2.50"} // same browser, new network
	if _, err := r.m.Get(context.Background(), id, moved); err != nil {
		t.Fatalf("Get from a new address = %v, want the session", err)
	}
}

// Without a token there is no owner, so there is nothing to admit. The HTTP
// layer refuses such a request before it gets here; reaching Admit is a bug.
func TestEmptyHashCannotCreate(t *testing.T) {
	r := newRig(t, roomy)
	if _, err := r.createAs(Owner{Client: alice.Client}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Create with no hash = %v, want ErrInvalid", err)
	}
}

func TestSecondCreateFromTheSameClientIsRefused(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 1, PerClient: 1})
	id := r.mustCreateAs(alice)

	_, err := r.createAs(alice)
	if q := quotaError(t, err); !q.Yours || q.ID != id {
		t.Errorf("same token: %+v, want Yours with ID %s", q, id)
	}

	_, err = r.createAs(bob)
	if q := quotaError(t, err); q.Yours || q.ID != "" || !q.ClientWide {
		t.Errorf("same address, other token: %+v, want ClientWide and neither Yours nor an ID", q)
	}

	if _, err := r.createAs(carol); err != nil {
		t.Errorf("another address = %v, want it unaffected", err)
	}
}

func TestQuotaPointsAtTheOldestOfSeveral(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 2, PerClient: 2})
	oldest := r.mustCreateAs(alice)
	r.clk.Advance(time.Minute)
	r.mustCreateAs(alice)

	_, err := r.createAs(alice)
	if q := quotaError(t, err); q.ID != oldest {
		t.Fatalf("ID = %q, want the oldest, %s", q.ID, oldest)
	}
}

// The ID is only ever returned to its owner, even when the caller holds none.
func TestQuotaNeverRevealsAnotherOwnersID(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 2, PerClient: 2})
	r.mustCreateAs(alice)
	r.mustCreateAs(bob)

	_, err := r.createAs(Owner{Hash: "dddd", Client: alice.Client})
	if q := quotaError(t, err); q.Yours || q.ID != "" {
		t.Fatalf("%+v, want neither Yours nor an ID", q)
	}
}

// One browser holds one live session, whichever network it comes from.
func TestOwnerLimitHoldsAcrossNetworks(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 1, PerClient: 1000})
	id := r.mustCreateAs(alice)

	moved := Owner{Hash: alice.Hash, Client: "192.0.2.50"}
	_, err := r.createAs(moved)
	if q := quotaError(t, err); !q.Yours || q.ID != id {
		t.Errorf("same token from another network: %+v, want Yours with ID %s", q, id)
	}
	r.mustCreateAs(bob) // same network as alice, another owner
}

// ADR 0010, section 13.3: one per user, three per address, so a shared
// network still works but cannot flood with many browsers.
func TestClientBackstopIsLooserThanOwnerLimit(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 1, PerClient: 3})
	dave := Owner{Hash: "dddd", Client: alice.Client}
	erin := Owner{Hash: "eeee", Client: alice.Client}

	for _, o := range []Owner{alice, bob, dave} {
		r.mustCreateAs(o)
	}
	_, err := r.createAs(erin)
	if q := quotaError(t, err); q.Yours || q.ID != "" || !q.ClientWide {
		t.Errorf("fourth owner on a full network: %+v, want ClientWide and not Yours", q)
	}
	r.mustCreateAs(carol) // another network is unaffected
}

// A session being deleted does not block its owner (ADR 0010, section 4).
func TestASessionBeingDeletedDoesNotCount(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 1, PerClient: 1})
	id := r.mustCreateAs(alice)
	if err := r.m.Delete(context.Background(), id, alice); err != nil {
		t.Fatal(err)
	}

	if _, err := r.createAs(bob); err != nil {
		t.Fatalf("Create after Delete = %v, want the quota free", err)
	}
}

// Meant for -race: the check and the insert must be one step.
func TestConcurrentCreatesFromOneClientMakeExactlyOneSession(t *testing.T) {
	r := newRig(t, Limits{Capacity: 10, PerOwner: 1, PerClient: 1})

	const n = 50
	start := make(chan struct{})
	errs := make(chan error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := alice
			if i%2 == 1 {
				o = bob
			}
			<-start
			_, err := r.m.Create(context.Background(), o)
			errs <- err
		}()
	}
	close(start)
	wg.Wait()
	close(errs)

	ok := 0
	for err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrQuota):
			t.Errorf("unexpected error: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d successes, want exactly 1", ok)
	}
}
