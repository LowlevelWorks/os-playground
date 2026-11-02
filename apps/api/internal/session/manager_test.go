package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

const (
	testImage = "/images/os.iso"
	testMemMB = 128
	idle      = 15 * time.Minute
	maxLife   = time.Hour
)

// alice and bob share one address; carol is elsewhere. Hashes are opaque to
// the manager, so short stand-ins do.
var (
	alice = Owner{Hash: "aaaa", Client: "203.0.113.1"}
	bob   = Owner{Hash: "bbbb", Client: "203.0.113.1"}
	carol = Owner{Hash: "cccc", Client: "198.51.100.7"}
)

// roomy is for tests that are not about limits, so they never meet one.
var roomy = Limits{Capacity: 1000, PerOwner: 1000, PerClient: 1000}

// clock is a hand-driven clock, so no test sleeps.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	t     *testing.T
	fake  *fake.Fake
	clk   *clock
	store *MemStore
	m     *Manager
}

// newRig builds a Manager over a MemStore and the fake provider, with a fake
// clock.
func newRig(t *testing.T, l Limits) *rig {
	t.Helper()
	clk := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	store := NewMemStore(clk.Now)
	f := fake.New(fake.WithAutoRun())
	m, err := New(Config{
		Store:        store,
		Provider:     f,
		Image:        testImage,
		MemoryMB:     testMemMB,
		Capacity:     l.Capacity,
		MaxPerOwner:  l.PerOwner,
		MaxPerClient: l.PerClient,
		IdleTimeout:  idle,
		MaxDuration:  maxLife,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &rig{t: t, fake: f, clk: clk, store: store, m: m}
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

// boot does what the reconciler does: it creates the session's VM and
// records it as RUNNING.
func (r *rig) boot(id string) {
	r.t.Helper()
	if _, err := r.fake.Create(context.Background(), vm.Spec{ID: id, Image: testImage, MemoryMB: testMemMB}); err != nil {
		r.t.Fatalf("boot %s: %v", id, err)
	}
	r.store.mu.Lock()
	r.store.rows[id].s.Status = StatusRunning
	r.store.mu.Unlock()
}

// vmExists asks the provider, not the manager.
func (r *rig) vmExists(id string) bool {
	_, err := r.fake.Get(context.Background(), id)
	return err == nil
}

// failingStore returns the given errors and embeds a nil Store, so a call to
// any other method panics: the test then shows it was not expected.
type failingStore struct {
	Store
	admitErr, getErr error
}

func (s failingStore) Admit(context.Context, NewSession, Limits) (Session, error) {
	return Session{}, s.admitErr
}

func (s failingStore) Get(context.Context, string, string) (Session, error) {
	return Session{}, s.getErr
}

func (s failingStore) AcquireAttach(context.Context, string, string, string, time.Duration) error {
	return s.getErr
}

func newOver(t *testing.T, s Store) *Manager {
	t.Helper()
	m, err := New(Config{
		Store: s, Provider: fake.New(), Image: testImage, MemoryMB: testMemMB,
		Capacity: 1, MaxPerOwner: 1, MaxPerClient: 1, IdleTimeout: idle, MaxDuration: maxLife,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func TestNewReportsEveryInvalidField(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New(Config{}) = nil, want errors")
	}
	for _, want := range []string{"Store", "Provider", "Image", "MemoryMB", "Capacity", "MaxPerOwner", "MaxPerClient", "IdleTimeout", "MaxDuration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

// ADR 0010, section 5: the row is committed and nothing else happens.
func TestCreateIsInsertOnly(t *testing.T) {
	r := newRig(t, roomy)

	inst, err := r.m.Create(context.Background(), alice)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := uuid.Parse(inst.ID); err != nil {
		t.Errorf("ID = %q, want a UUID: %v", inst.ID, err)
	}
	if inst.Status != vm.StatusProvisioning {
		t.Errorf("Status = %q, want %q", inst.Status, vm.StatusProvisioning)
	}
	if !inst.CreatedAt.Equal(r.clk.Now()) {
		t.Errorf("CreatedAt = %v, want %v", inst.CreatedAt, r.clk.Now())
	}
	if r.fake.Len() != 0 {
		t.Errorf("the provider holds %d VMs, want 0: Create must not call it", r.fake.Len())
	}

	other := r.mustCreateAs(alice)
	if other == inst.ID {
		t.Error("two sessions got the same ID")
	}
}

func TestCreateChoosesTheSpecItself(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)

	s, err := r.store.Get(context.Background(), id, alice.Hash)
	if err != nil {
		t.Fatalf("store.Get: %v", err)
	}
	if s.Status != StatusCreated || s.Profile != defaultProfile || s.ImageRef != testImage || s.ClientKey != alice.Client {
		t.Errorf("stored %+v, want CREATED, profile %q, image %q, client %q", s, defaultProfile, testImage, alice.Client)
	}
	var spec vmSpec
	if err := json.Unmarshal(s.Spec, &spec); err != nil || spec.MemoryMB != testMemMB {
		t.Errorf("Spec = %s (%v), want memory_mb %d", s.Spec, err, testMemMB)
	}
	if got := s.ExpiresAt.Sub(s.CreatedAt); got != maxLife {
		t.Errorf("ExpiresAt - CreatedAt = %v, want %v", got, maxLife)
	}
	if got := s.IdleExpiresAt.Sub(s.CreatedAt); got != idle {
		t.Errorf("IdleExpiresAt - CreatedAt = %v, want %v", got, idle)
	}
}

// The handler tells these errors apart, so the manager must not wrap or
// replace them.
func TestCreatePassesStoreErrorsThrough(t *testing.T) {
	quota := &QuotaError{Yours: true, ID: "x"}
	for _, want := range []error{ErrBusy, ErrCapacity, quota, ErrInvalid} {
		_, err := newOver(t, failingStore{admitErr: want}).Create(context.Background(), alice)
		if !errors.Is(err, want) {
			t.Errorf("Create = %v, want %v", err, want)
		}
	}

	_, err := newOver(t, failingStore{admitErr: quota}).Create(context.Background(), alice)
	var q *QuotaError
	if !errors.As(err, &q) || q != quota {
		t.Errorf("Create = %#v, want the store's *QuotaError itself", err)
	}
}

// A database fault must reach the handler as a fault (500), not as "not
// found" (404), or an outage looks like every session vanished.
func TestStoreFaultIsNotNotFound(t *testing.T) {
	m := newOver(t, failingStore{getErr: errors.New("connection refused")})
	ctx := context.Background()

	_, getErr := m.Get(ctx, "id", alice)
	_, consoleErr := m.Console(ctx, "id", alice)
	deleteErr := m.Delete(ctx, "id", alice)
	for name, err := range map[string]error{"Get": getErr, "Console": consoleErr, "Delete": deleteErr} {
		if err == nil || errors.Is(err, vm.ErrNotFound) || !strings.Contains(err.Error(), "connection refused") {
			t.Errorf("%s = %v, want the store's error, not ErrNotFound", name, err)
		}
	}
}

func TestPublicStatus(t *testing.T) {
	tests := map[Status]vm.Status{
		StatusCreated:      vm.StatusProvisioning,
		StatusProvisioning: vm.StatusProvisioning,
		StatusStarting:     vm.StatusStarting,
		StatusRunning:      vm.StatusRunning,
		StatusStopping:     vm.StatusUnknown,
		StatusStopped:      vm.StatusStopped,
		StatusFailed:       vm.StatusFailed,
		Status("NEW"):      vm.StatusUnknown,
	}
	for in, want := range tests {
		if got := in.public(); got != want {
			t.Errorf("%s.public() = %q, want %q", in, got, want)
		}
	}
}

// ADR 0010, section 4: a deleted session is invisible the moment it is
// marked. Removing the VM is the reconciler's job, not Delete's.
func TestDeleteHidesAtOnceAndLeavesTheVMToTheReconciler(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)
	r.boot(id)
	ctx := context.Background()

	if err := r.m.Delete(ctx, id, alice); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := r.m.Get(ctx, id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := r.m.Console(ctx, id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Console after Delete = %v, want ErrNotFound", err)
	}
	if err := r.m.Delete(ctx, id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("second Delete = %v, want ErrNotFound", err)
	}
	if !r.vmExists(id) {
		t.Error("Delete called the provider; only the reconciler may")
	}
}

// Quota is freed when Delete returns; capacity only when the VM is gone.
func TestDeleteFreesQuotaButNotCapacity(t *testing.T) {
	r := newRig(t, Limits{Capacity: 2, PerOwner: 1, PerClient: 1})
	id := r.mustCreateAs(alice)
	if err := r.m.Delete(context.Background(), id, alice); err != nil {
		t.Fatal(err)
	}

	r.mustCreateAs(alice)
	if _, err := r.createAs(carol); !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create = %v, want ErrCapacity: the deleted session's VM is not released yet", err)
	}
}

func TestConsoleReachesTheVMOnceItExists(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)
	ctx := context.Background()

	if _, err := r.m.Console(ctx, id, alice); !errors.Is(err, vm.ErrNotRunning) {
		t.Fatalf("Console before the session is RUNNING = %v, want ErrNotRunning", err)
	}

	r.boot(id)
	c, err := r.m.Console(ctx, id, alice)
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// Another spelling of the same UUID must reach the same VM: the provider
// is asked by the stored ID, never by the caller's string.
func TestConsoleUsesTheStoredID(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)
	r.boot(id)

	c, err := r.m.Console(context.Background(), strings.ToUpper(id), alice)
	if err != nil {
		t.Fatalf("Console with an upper-case ID: %v", err)
	}
	c.Close()
}

// Meant for -race: only deadlocks and races fail this.
func TestConcurrentUse(t *testing.T) {
	r := newRig(t, roomy)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := Owner{Hash: "owner" + strconv.Itoa(w), Client: alice.Client}
			ctx := context.Background()
			for range 20 {
				inst, err := r.m.Create(ctx, o)
				if err != nil {
					t.Errorf("Create: %v", err)
					return
				}
				_, _ = r.m.Get(ctx, inst.ID, o)
				if c, err := r.m.Console(ctx, inst.ID, o); err == nil {
					c.Close()
				}
				if err := r.m.Delete(ctx, inst.ID, o); err != nil {
					t.Errorf("Delete: %v", err)
				}
			}
		}()
	}
	wg.Wait()
}

// runningSession gives alice a RUNNING session with a VM.
func (r *rig) runningSession() string {
	r.t.Helper()
	id := r.mustCreateAs(alice)
	r.boot(id)
	return id
}

// The lease, not the provider, refuses a second console: it holds across
// replicas, which a provider's own limit cannot.
func TestSecondConsoleIsRefusedByTheLease(t *testing.T) {
	r := newRig(t, roomy)
	id := r.runningSession()
	ctx := context.Background()

	first, err := r.m.Console(ctx, id, alice)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.m.Console(ctx, id, alice); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Fatalf("second Console = %v, want ErrAlreadyAttached", err)
	}

	first.Close()
	waitFor(t, func() bool {
		c, err := r.m.Console(ctx, id, alice)
		if err == nil {
			c.Close()
		}
		return err == nil
	})
}

func TestFailedProviderAttachGivesTheLeaseBack(t *testing.T) {
	r := newRig(t, roomy)
	id := r.mustCreateAs(alice)
	r.store.mu.Lock()
	r.store.rows[id].s.Status = StatusRunning // RUNNING in the row, but no VM
	r.store.mu.Unlock()

	if _, err := r.m.Console(context.Background(), id, alice); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Console = %v, want the provider's ErrNotFound", err)
	}
	waitFor(t, func() bool {
		return r.store.AcquireAttach(context.Background(), id, alice.Hash, "other", time.Minute) == nil
	})
}

// Deleting the session takes the lease away at the next renewal, which ends
// the console even if the VM were slow to go.
func TestLostLeaseEndsTheConsole(t *testing.T) {
	r := newRig(t, roomy)
	r.m.attachRenew = 5 * time.Millisecond
	id := r.runningSession()

	c, err := r.m.Console(context.Background(), id, alice)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(ended)
	}()

	if err := r.m.Delete(context.Background(), id, alice); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the console outlived its lease")
	}
}

// flakyRenew fails RenewAttach while down is set, as a database blip would.
type flakyRenew struct {
	*MemStore
	mu   sync.Mutex
	down bool
}

func (s *flakyRenew) setDown(v bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = v
}

func (s *flakyRenew) RenewAttach(ctx context.Context, id, holder string, lease time.Duration) error {
	s.mu.Lock()
	down := s.down
	s.mu.Unlock()
	if down {
		return errors.New("connection refused")
	}
	return s.MemStore.RenewAttach(ctx, id, holder, lease)
}

func TestDatabaseBlipShorterThanTheLeaseKeepsTheConsole(t *testing.T) {
	r := newRig(t, roomy)
	store := &flakyRenew{MemStore: r.store}
	r.m.store = store
	r.m.attachLease, r.m.attachRenew = 300*time.Millisecond, 5*time.Millisecond
	id := r.runningSession()

	c, err := r.m.Console(context.Background(), id, alice)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ended := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, c)
		close(ended)
	}()

	store.setDown(true)
	time.Sleep(50 * time.Millisecond) // a blip: well inside the lease
	store.setDown(false)
	time.Sleep(50 * time.Millisecond)
	select {
	case <-ended:
		t.Fatal("a short blip dropped the console")
	default:
	}

	store.setDown(true) // an outage: longer than the lease
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the console outlived a lease it could not renew")
	}
}
