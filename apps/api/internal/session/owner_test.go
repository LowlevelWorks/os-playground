package session

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

// newQuotaRig is newRig with a per-client limit of n.
func newQuotaRig(t *testing.T, n int, opts ...fake.Option) *rig {
	t.Helper()
	r := newRig(t, opts...)
	r.m.perClient = n
	return r
}

func (r *rig) createAs(o Owner) (string, error) {
	r.t.Helper()
	inst, err := r.m.Create(context.Background(), o)
	return inst.ID, err
}

func (r *rig) mustCreateAs(o Owner) string {
	r.t.Helper()
	id, err := r.createAs(o)
	if err != nil {
		r.t.Fatalf("Create as %s: %v", o.Hash, err)
	}
	return id
}

func (r *rig) pendingClients() int {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return len(r.m.pending)
}

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
	r := newRig(t)
	id := r.mustCreateAs(alice)
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

	if !r.vmExists(id) {
		t.Fatal("another owner's Delete removed the VM")
	}
	if _, err := r.m.Get(ctx, id, alice); err != nil {
		t.Fatalf("the owner lost access: %v", err)
	}
	// A refused attach must not leave an attach reservation behind.
	r.reapAfter(idle)
	if r.vmExists(id) {
		t.Fatal("a refused attach kept the VM from going idle")
	}
}

func TestTheOwnerIsTheTokenNotTheAddress(t *testing.T) {
	r := newRig(t)
	id := r.mustCreateAs(alice)

	moved := Owner{Hash: alice.Hash, Client: "192.0.2.50"} // same browser, new network
	if _, err := r.m.Get(context.Background(), id, moved); err != nil {
		t.Fatalf("Get from a new address = %v, want the VM", err)
	}
}

func TestSecondCreateFromTheSameClientIsRefused(t *testing.T) {
	r := newQuotaRig(t, 1)
	id := r.mustCreateAs(alice)

	_, err := r.createAs(alice)
	if q := quotaError(t, err); !q.Yours || q.ID != id {
		t.Errorf("same token: %+v, want Yours with ID %s", q, id)
	}

	_, err = r.createAs(bob)
	if q := quotaError(t, err); q.Yours || q.ID != "" {
		t.Errorf("same address, other token: %+v, want neither Yours nor an ID", q)
	}

	if _, err := r.createAs(carol); err != nil {
		t.Errorf("another address = %v, want it unaffected", err)
	}
	if r.fake.Len() != 2 {
		t.Errorf("provider holds %d VMs, want 2: a refused create reached it", r.fake.Len())
	}
}

func TestQuotaPointsAtTheOldestOfSeveral(t *testing.T) {
	r := newQuotaRig(t, 2)
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
	r := newQuotaRig(t, 2)
	r.mustCreateAs(alice)
	r.mustCreateAs(bob)

	_, err := r.createAs(Owner{Hash: "dddd", Client: alice.Client})
	if q := quotaError(t, err); q.Yours || q.ID != "" {
		t.Fatalf("%+v, want neither Yours nor an ID", q)
	}
}

// heldCreate blocks the first Create until the test says how it ends. Later
// creates go straight through.
type heldCreate struct {
	vm.Provider
	entered chan struct{}
	finish  chan error // nil: let the real provider create the VM

	mu   sync.Mutex
	used bool
}

func newHeldCreate(p vm.Provider) *heldCreate {
	return &heldCreate{Provider: p, entered: make(chan struct{}), finish: make(chan error)}
}

func (h *heldCreate) Create(ctx context.Context, spec vm.Spec) (vm.Instance, error) {
	h.mu.Lock()
	first := !h.used
	h.used = true
	h.mu.Unlock()

	if first {
		close(h.entered)
		if err := <-h.finish; err != nil {
			return vm.Instance{}, err
		}
	}
	return h.Provider.Create(ctx, spec)
}

// startHeld begins a create that blocks inside the provider and returns its
// result channel.
func startHeld(r *rig, h *heldCreate, o Owner) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := r.m.Create(context.Background(), o)
		done <- err
	}()
	<-h.entered
	return done
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		t.Fatal("Create did not return")
		return nil
	}
}

func TestACreateInFlightCountsTowardsTheQuota(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	h := newHeldCreate(f)
	r := newRigOver(t, f, h)
	r.m.perClient = 1
	done := startHeld(r, h, alice)

	_, err := r.createAs(alice)
	if q := quotaError(t, err); !q.Yours || q.ID != "" {
		t.Errorf("same token while its create is in flight: %+v, want Yours and no ID", q)
	}
	_, err = r.createAs(bob)
	if q := quotaError(t, err); q.Yours {
		t.Errorf("other token while alice's create is in flight: %+v, want not Yours", q)
	}

	h.finish <- nil
	if err := wait(t, done); err != nil {
		t.Fatalf("the held create: %v", err)
	}
	if r.pendingClients() != 0 {
		t.Error("the reservation outlived the create")
	}
	_, err = r.createAs(alice)
	if q := quotaError(t, err); !q.Yours || q.ID == "" {
		t.Errorf("after the create finished: %+v, want Yours with the ID", q)
	}
}

func TestAFailedCreateGivesTheSlotBack(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	h := newHeldCreate(f)
	r := newRigOver(t, f, h)
	r.m.perClient = 1
	done := startHeld(r, h, alice)

	h.finish <- errors.New("qemu would not start")
	if err := wait(t, done); err == nil {
		t.Fatal("the held create succeeded, want its error")
	}

	if r.pendingClients() != 0 || r.tracked() != 0 {
		t.Fatalf("after a failed create: %d pending clients, %d sessions; want none", r.pendingClients(), r.tracked())
	}
	if _, err := r.createAs(bob); err != nil {
		t.Fatalf("Create after a failed create = %v, want the slot free", err)
	}
}

func TestCapacityErrorGivesTheSlotBack(t *testing.T) {
	r := newQuotaRig(t, 1, fake.WithCapacity(1))
	r.mustCreateAs(carol) // fills the provider

	if _, err := r.createAs(alice); !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create = %v, want ErrCapacity", err)
	}
	if r.pendingClients() != 0 {
		t.Fatal("a refused create kept its reservation")
	}
}

// Meant for -race: the check and the reservation must be one step.
func TestConcurrentCreatesFromOneClientMakeExactlyOneVM(t *testing.T) {
	r := newQuotaRig(t, 1)

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
	if ok != 1 || r.fake.Len() != 1 || r.tracked() != 1 {
		t.Fatalf("%d successes, %d VMs, %d sessions; want exactly 1 of each", ok, r.fake.Len(), r.tracked())
	}
	if r.pendingClients() != 0 {
		t.Fatal("reservations left behind")
	}
}

func TestDeletingFreesTheSlot(t *testing.T) {
	r := newQuotaRig(t, 1)
	id := r.mustCreateAs(alice)

	if err := r.m.Delete(context.Background(), id, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := r.createAs(bob); err != nil {
		t.Fatalf("Create after Delete = %v, want the slot free", err)
	}
}

// A Delete that has not finished yet still frees the slot: the VM is hidden
// and on its way out, and the caller should not wait on the provider.
func TestAVMBeingDeletedDoesNotCount(t *testing.T) {
	r := newQuotaRig(t, 1)
	id := r.mustCreateAs(alice)
	// Hidden, still alive: the cancelled ctx abandons the delete part-way.
	if err := r.m.Delete(cancelled(), id, alice); !errors.Is(err, context.Canceled) {
		t.Fatalf("setup: Delete with a cancelled ctx = %v, want context.Canceled", err)
	}

	if !r.vmExists(id) {
		t.Fatal("setup: the VM should still exist")
	}
	if _, err := r.createAs(alice); err != nil {
		t.Fatalf("Create while the old VM is being deleted = %v, want success", err)
	}
}

// A browser that lost its token (cleared storage, another browser) is a new
// owner on the same address. It waits for the reaper, and then gets a VM.
func TestReapingFreesTheSlotForAnotherTokenOnTheSameAddress(t *testing.T) {
	r := newQuotaRig(t, 1)
	r.mustCreateAs(alice)

	if _, err := r.createAs(bob); !errors.Is(err, ErrQuota) {
		t.Fatalf("Create before the reap = %v, want ErrQuota", err)
	}

	r.reapAfter(idle)

	if r.tracked() != 0 {
		t.Fatal("setup: the reaper should have removed the idle VM")
	}
	if _, err := r.createAs(bob); err != nil {
		t.Fatalf("Create after the reap = %v, want success", err)
	}
}

func TestFinishedVMFreesTheSlotOnceReaped(t *testing.T) {
	r := newQuotaRig(t, 1)
	id := r.mustCreateAs(alice)
	if err := r.fake.Finish(id, vm.StatusStopped, "halted"); err != nil {
		t.Fatal(err)
	}

	r.reapAfter(0)
	if _, err := r.createAs(alice); !errors.Is(err, ErrQuota) {
		t.Fatalf("Create inside the grace period = %v, want ErrQuota", err)
	}
	r.reapAfter(grace)
	if _, err := r.createAs(alice); err != nil {
		t.Fatalf("Create after the grace period = %v, want success", err)
	}
}
