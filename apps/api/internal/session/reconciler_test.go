package session

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

const visibleFor = 5 * time.Minute

// recRig is a Manager and a Reconciler sharing one MemStore, provider and
// hand-driven clock. VMs move only when the test advances them.
type recRig struct {
	t     *testing.T
	clk   *clock
	fake  *fake.Fake
	store *MemStore
	m     *Manager
	rec   *Reconciler
}

func newRecRig(t *testing.T, l Limits, wrap func(vm.Provider) vm.Provider) *recRig {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	f := fake.New(fake.WithClock(clk.Now))
	var p vm.Provider = f
	if wrap != nil {
		p = wrap(f)
	}
	store := NewMemStore(clk.Now)
	m, err := New(Config{
		Store: store, Provider: p, Image: testImage, MemoryMB: testMemMB,
		Capacity: l.Capacity, MaxPerOwner: l.PerOwner, MaxPerClient: l.PerClient,
		IdleTimeout: idle, MaxDuration: maxLife,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.now = clk.Now
	rec, err := NewReconciler(ReconcilerConfig{
		Store: store, Provider: p, FailedVisibleFor: visibleFor, Worker: "test", MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("NewReconciler: %v", err)
	}
	rec.now = clk.Now
	return &recRig{t: t, clk: clk, fake: f, store: store, m: m, rec: rec}
}

// pass moves the clock by d, then runs one reconcile pass.
func (r *recRig) pass(d time.Duration) {
	r.t.Helper()
	r.clk.Advance(d)
	if _, err := r.rec.ReconcileOnce(context.Background()); err != nil {
		r.t.Fatalf("ReconcileOnce: %v", err)
	}
}

func (r *recRig) create(o Owner) string {
	r.t.Helper()
	inst, err := r.m.Create(context.Background(), o)
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	return inst.ID
}

// stored reads the row whatever its state, as the reconciler sees it.
func (r *recRig) stored(id string) Session {
	r.t.Helper()
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	row, ok := r.store.rows[id]
	if !ok {
		r.t.Fatalf("no row %s", id)
	}
	return row.copy()
}

func (r *recRig) released(id string) bool {
	r.store.mu.Lock()
	defer r.store.mu.Unlock()
	return !r.store.rows[id].vmReleasedAt.IsZero()
}

func (r *recRig) vmExists(id string) bool {
	_, err := r.fake.Get(context.Background(), id)
	return err == nil
}

func (r *recRig) wantStatus(id string, want Status) {
	r.t.Helper()
	if got := r.stored(id).Status; got != want {
		r.t.Fatalf("status = %s, want %s", got, want)
	}
}

// running creates a session and drives it, and its VM, to RUNNING.
func (r *recRig) running(o Owner) string {
	r.t.Helper()
	id := r.create(o)
	r.pass(0) // CREATED -> PROVISIONING: the VM is created
	for range 2 {
		if err := r.fake.Advance(id); err != nil {
			r.t.Fatal(err)
		}
	}
	r.pass(changingRetry)
	r.wantStatus(id, StatusRunning)
	return id
}

func TestNewReconcilerChecksItsConfig(t *testing.T) {
	_, err := NewReconciler(ReconcilerConfig{Batch: -1})
	if err == nil {
		t.Fatal("NewReconciler(bad) = nil error")
	}
	for _, want := range []string{"Store", "Provider", "FailedVisibleFor", "Batch"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}

	_, err = NewReconciler(ReconcilerConfig{
		Store: NewMemStore(time.Now), Provider: fake.New(), FailedVisibleFor: time.Minute,
		Lease: time.Second, StepTimeout: 2 * time.Second,
	})
	if err == nil || !strings.Contains(err.Error(), "Lease") {
		t.Errorf("a lease shorter than a step = %v, want an error", err)
	}
}

func TestReconcilerBootsACreatedSession(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.create(alice)
	if r.vmExists(id) {
		t.Fatal("setup: Create made a VM")
	}

	r.pass(0)
	if !r.vmExists(id) {
		t.Fatal("the reconciler did not create the VM")
	}
	r.wantStatus(id, StatusProvisioning)

	if err := r.fake.Advance(id); err != nil {
		t.Fatal(err)
	}
	r.pass(changingRetry)
	r.wantStatus(id, StatusStarting)

	if err := r.fake.Advance(id); err != nil {
		t.Fatal(err)
	}
	r.pass(changingRetry)
	inst, err := r.m.Get(context.Background(), id, alice)
	if err != nil || inst.Status != vm.StatusRunning {
		t.Fatalf("Get = %+v, %v; want running", inst, err)
	}
}

func TestDeleteRemovesTheVMAndThenFreesCapacity(t *testing.T) {
	r := newRecRig(t, Limits{Capacity: 1, PerOwner: 5, PerClient: 5}, nil)
	id := r.running(alice)

	if err := r.m.Delete(context.Background(), id, alice); err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Create(context.Background(), carol); !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create before the VM is gone = %v, want ErrCapacity", err)
	}

	r.pass(0)
	if r.vmExists(id) {
		t.Fatal("the VM survived the reconciler")
	}
	r.wantStatus(id, StatusDeleted)
	if !r.released(id) {
		t.Fatal("vm_released_at not set")
	}
	r.create(carol)
}

// A provider that fails after doing the work: the "created but unrecorded"
// window of ADR 0010, section 6.3.
type createsThenFails struct {
	vm.Provider
	mu    sync.Mutex
	fired bool
}

func (p *createsThenFails) Create(ctx context.Context, spec vm.Spec) (vm.Instance, error) {
	inst, err := p.Provider.Create(ctx, spec)
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil && !p.fired {
		p.fired = true
		return vm.Instance{}, errors.New("connection reset after the VM was created")
	}
	return inst, err
}

func TestRetriedCreateTakesAlreadyExistsAsSuccess(t *testing.T) {
	r := newRecRig(t, roomy, func(p vm.Provider) vm.Provider { return &createsThenFails{Provider: p} })
	id := r.create(alice)

	r.pass(0)
	r.wantStatus(id, StatusCreated)
	if !r.vmExists(id) {
		t.Fatal("setup: the VM should exist despite the error")
	}

	r.pass(maxBackoff)
	r.wantStatus(id, StatusProvisioning)
	if r.fake.Len() != 1 {
		t.Fatalf("%d VMs, want 1", r.fake.Len())
	}
}

// alwaysFull refuses every create, as a cluster with no room would.
type alwaysFull struct{ vm.Provider }

func (alwaysFull) Create(context.Context, vm.Spec) (vm.Instance, error) {
	return vm.Instance{}, vm.ErrCapacity
}

func TestCreateGivesUpAfterMaxAttemptsAndReleases(t *testing.T) {
	r := newRecRig(t, Limits{Capacity: 1, PerOwner: 5, PerClient: 5}, func(p vm.Provider) vm.Provider { return alwaysFull{p} })
	id := r.create(alice)

	r.pass(0)
	r.pass(maxBackoff)
	r.wantStatus(id, StatusCreated)
	r.pass(maxBackoff) // third attempt: MaxAttempts is 3

	s := r.stored(id)
	if s.Status != StatusFailed || !strings.Contains(s.FailureReason, "capacity") {
		t.Fatalf("session = %s %q, want FAILED with the provider's reason", s.Status, s.FailureReason)
	}
	r.pass(0)
	if !r.released(id) {
		t.Fatal("a session that never got a VM still holds capacity")
	}
	r.create(carol)
}

func TestVanishedVMFailsTheSession(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.running(alice)
	if err := r.fake.Delete(context.Background(), id); err != nil { // node loss, or a restart
		t.Fatal(err)
	}

	r.pass(runningRetry)

	inst, err := r.m.Get(context.Background(), id, alice)
	if err != nil || inst.Status != vm.StatusFailed || inst.Reason != "vm disappeared" {
		t.Fatalf("Get = %+v, %v; want failed, vm disappeared", inst, err)
	}
}

// ADR 0010, section 4: the VM goes at once, the session stays readable for
// FailedVisibleFor, then it is DELETED.
func TestFailedSessionReleasesAtOnceThenDisappears(t *testing.T) {
	r := newRecRig(t, Limits{Capacity: 1, PerOwner: 5, PerClient: 5}, nil)
	id := r.running(alice)
	if err := r.fake.Finish(id, vm.StatusFailed, "kernel panic"); err != nil {
		t.Fatal(err)
	}

	r.pass(runningRetry)
	r.wantStatus(id, StatusFailed)
	r.pass(0)
	if r.vmExists(id) || !r.released(id) {
		t.Fatal("the failed session's VM was not released at once")
	}
	r.create(carol) // the slot is free

	r.pass(visibleFor - time.Second)
	if inst, err := r.m.Get(context.Background(), id, alice); err != nil || inst.Reason != "kernel panic" {
		t.Fatalf("inside the visible period: %+v, %v; want the failed session", inst, err)
	}
	r.pass(time.Second)
	if _, err := r.m.Get(context.Background(), id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("after the visible period: %v, want ErrNotFound", err)
	}
	r.wantStatus(id, StatusDeleted)
}

func TestStoppedVMIsTreatedLikeAFailedOne(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.running(alice)
	if err := r.fake.Finish(id, vm.StatusStopped, "halted"); err != nil {
		t.Fatal(err)
	}

	r.pass(runningRetry)
	r.pass(0)
	r.wantStatus(id, StatusStopped)
	if r.vmExists(id) || !r.released(id) {
		t.Fatal("the stopped session's VM was not released")
	}
}

func TestMaxDurationEndsTheSession(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.running(alice)

	r.pass(maxLife)
	if _, err := r.m.Get(context.Background(), id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Get after expiry = %v, want ErrNotFound at once", err)
	}
	r.pass(0)
	if r.vmExists(id) {
		t.Fatal("an expired session's VM survived")
	}
	r.wantStatus(id, StatusDeleted)
}

func TestNeverUsedSessionExpiresWhenIdle(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.running(alice)

	r.pass(idle - changingRetry - time.Second) // running() already moved the clock by changingRetry
	if r.stored(id).DesiredDeleted {
		t.Fatal("expired before the idle timeout")
	}
	// Running sessions are checked every runningRetry: that is the
	// precision of expiry (ADR 0010, section 6.2).
	r.pass(runningRetry)
	if !r.stored(id).DesiredDeleted {
		t.Fatal("not expired within runningRetry of the idle timeout")
	}
}

// Console input pushes the idle deadline out; the write is asynchronous, so
// the test waits for it to land.
func TestConsoleInputKeepsTheSessionAlive(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.running(alice)
	before := r.stored(id).IdleExpiresAt

	c, err := r.m.Console(context.Background(), id, alice)
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	defer c.Close()
	guest, err := r.fake.Guest(id)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _, _ = io.Copy(io.Discard, guest) }()

	r.clk.Advance(10 * time.Minute)
	if _, err := c.Write([]byte("ls\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	waitFor(t, func() bool { return r.stored(id).IdleExpiresAt.After(before) })

	r.pass(idle - time.Minute) // past the original deadline
	if r.stored(id).DesiredDeleted {
		t.Fatal("expired as idle despite console input")
	}
}

func TestConsoleActivityIsThrottled(t *testing.T) {
	store := &countingStore{Store: NewMemStore(time.Now)}
	clk := &clock{t: time.Now()}
	m := newOver(t, store)
	m.now = clk.Now
	c := &activeConsole{ReadWriteCloser: nopRWC{}, m: m, id: "id"}
	write := func(p string) {
		t.Helper()
		if _, err := c.Write([]byte(p)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	write("a")
	waitFor(t, func() bool { return store.touches() == 1 })
	write("b")
	clk.Advance(activityThrottle - time.Second)
	write("c")
	clk.Advance(time.Second)
	write("d")
	waitFor(t, func() bool { return store.touches() == 2 })
	if n := store.touches(); n != 2 {
		t.Fatalf("%d touches, want 2", n)
	}
}

type countingStore struct {
	Store
	mu sync.Mutex
	n  int
}

func (s *countingStore) TouchActivity(context.Context, string, time.Duration) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	return nil
}

func (s *countingStore) touches() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

type nopRWC struct{}

func (nopRWC) Read([]byte) (int, error)    { return 0, io.EOF }
func (nopRWC) Write(p []byte) (int, error) { return len(p), nil }
func (nopRWC) Close() error                { return nil }

// countingCreates counts provider creates per ID.
type countingCreates struct {
	vm.Provider
	mu sync.Mutex
	n  map[string]int
}

func (p *countingCreates) Create(ctx context.Context, spec vm.Spec) (vm.Instance, error) {
	p.mu.Lock()
	p.n[spec.ID]++
	p.mu.Unlock()
	return p.Provider.Create(ctx, spec)
}

// Two replicas on one store: each session is worked on by one of them.
func TestTwoReconcilersNeverDoubleCreate(t *testing.T) {
	counter := &countingCreates{n: map[string]int{}}
	r := newRecRig(t, roomy, func(p vm.Provider) vm.Provider { counter.Provider = p; return counter })
	other, err := NewReconciler(ReconcilerConfig{Store: r.store, Provider: counter, FailedVisibleFor: visibleFor, Worker: "other", Batch: 2})
	if err != nil {
		t.Fatal(err)
	}
	other.now = r.clk.Now

	const n = 20
	for i := range n {
		r.create(Owner{Hash: "owner" + string(rune('a'+i)), Client: carol.Client})
	}

	var wg sync.WaitGroup
	for _, rec := range []*Reconciler{r.rec, other} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range n {
				if _, err := rec.ReconcileOnce(context.Background()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()

	if len(counter.n) != n {
		t.Fatalf("%d sessions got a VM, want %d", len(counter.n), n)
	}
	for id, c := range counter.n {
		if c != 1 {
			t.Errorf("session %s: %d creates, want 1", id, c)
		}
	}
}

func TestRunWakesOnCreateAndStopsWithItsContext(t *testing.T) {
	store := NewMemStore(time.Now)
	f := fake.New()
	rec, err := NewReconciler(ReconcilerConfig{Store: store, Provider: f, FailedVisibleFor: visibleFor, Interval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	m, err := New(Config{
		Store: store, Provider: f, Image: testImage, MemoryMB: testMemMB,
		Capacity: 5, MaxPerOwner: 5, MaxPerClient: 5, IdleTimeout: idle, MaxDuration: maxLife,
		Wake: rec.Wake,
	})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		rec.Run(ctx)
		close(stopped)
	}()

	// Run's first pass may come before or after this Create; with an
	// hour-long tick, only Wake can explain the VM appearing quickly.
	time.Sleep(10 * time.Millisecond)
	inst, err := m.Create(context.Background(), alice)
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, err := f.Get(context.Background(), inst.ID); return err == nil })

	cancel()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop when its context ended")
	}
}

func TestRetryBackoffIsBoundedAndJittered(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	for attempts := range 20 {
		want := min(changingRetry<<min(attempts, 10), maxBackoff)
		got := r.rec.retry(Claim{Attempts: attempts})
		if !got.Failed || got.RetryAfter < want/2 || got.RetryAfter > want {
			t.Errorf("attempts %d: %+v, want Failed and RetryAfter in [%v, %v]", attempts, got, want/2, want)
		}
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestSweepDeletesOnlyOrphans(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	kept := r.running(alice)
	ctx := context.Background()
	for _, id := range []string{"stranger", "01a19c9b-e5dc-7042-9485-a0f932e85580"} { // no row; a UUID with no row
		if _, err := r.fake.Create(ctx, vm.Spec{ID: id, Image: testImage, MemoryMB: testMemMB}); err != nil {
			t.Fatal(err)
		}
	}

	n, err := r.rec.SweepOrphans(ctx)
	if err != nil || n != 2 {
		t.Fatalf("SweepOrphans = %d, %v; want 2 deleted", n, err)
	}
	if !r.vmExists(kept) || r.fake.Len() != 1 {
		t.Fatalf("after the sweep: kept exists=%v, %d VMs; want only the session's VM", r.vmExists(kept), r.fake.Len())
	}
}

// A session's VM is created only after its row commits, so a sweep can
// never catch a VM between creation and record.
func TestSweepKeepsAVMWhoseSessionIsStillStarting(t *testing.T) {
	r := newRecRig(t, roomy, nil)
	id := r.create(alice)
	r.pass(0) // VM created, row PROVISIONING

	if n, err := r.rec.SweepOrphans(context.Background()); err != nil || n != 0 || !r.vmExists(id) {
		t.Fatalf("SweepOrphans = %d, %v, VM exists=%v; want the VM kept", n, err, r.vmExists(id))
	}
}
