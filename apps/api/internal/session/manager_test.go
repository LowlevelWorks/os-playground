package session

import (
	"context"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

const (
	testImage = "/images/os.iso"
	idle      = 15 * time.Minute
	maxLife   = time.Hour
	grace     = 30 * time.Second
	timeout   = 5 * time.Second
)

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
	t    *testing.T
	fake *fake.Fake
	clk  *clock
	m    *Manager
}

// newRig builds a Manager over the fake provider with a fake clock and
// sequential IDs (vm-1, vm-2, ...).
func newRig(t *testing.T, opts ...fake.Option) *rig {
	t.Helper()
	return newRigOver(t, fake.New(append([]fake.Option{fake.WithAutoRun()}, opts...)...), nil)
}

func newRigOver(t *testing.T, f *fake.Fake, p vm.Provider) *rig {
	t.Helper()
	if p == nil {
		p = f
	}
	m, err := New(Config{
		Provider:    p,
		Image:       testImage,
		MemoryMB:    128,
		IdleTimeout: idle,
		MaxDuration: maxLife,
		Grace:       grace,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	clk := &clock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	m.now = clk.Now
	n := 0
	var mu sync.Mutex
	m.newID = func() string {
		mu.Lock()
		defer mu.Unlock()
		n++
		return "vm-" + string(rune('0'+n%10)) + strings.Repeat("a", n/10)
	}
	return &rig{t: t, fake: f, clk: clk, m: m}
}

func (r *rig) create() string {
	r.t.Helper()
	inst, err := r.m.Create(context.Background())
	if err != nil {
		r.t.Fatalf("Create: %v", err)
	}
	return inst.ID
}

func (r *rig) attach(id string) io.ReadWriteCloser {
	r.t.Helper()
	c, err := r.m.Console(context.Background(), id)
	if err != nil {
		r.t.Fatalf("Console(%s): %v", id, err)
	}
	return c
}

// reapAfter moves the clock and runs one pass.
func (r *rig) reapAfter(d time.Duration) {
	r.clk.Advance(d)
	r.m.Reap(context.Background())
}

// vmExists asks the provider, not the manager.
func (r *rig) vmExists(id string) bool {
	_, err := r.fake.Get(context.Background(), id)
	return err == nil
}

func (r *rig) tracked() int {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	return len(r.m.sessions)
}

func cancelled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func TestNewRejectsAConfigurationThatWouldDeleteEverything(t *testing.T) {
	_, err := New(Config{})
	if err == nil {
		t.Fatal("New(Config{}) = nil, want errors")
	}
	for _, want := range []string{"Provider", "Image", "MemoryMB", "IdleTimeout", "MaxDuration"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestCreateChoosesTheSpecItself(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	m, err := New(Config{Provider: f, Image: testImage, MemoryMB: 128, IdleTimeout: idle, MaxDuration: maxLife})
	if err != nil {
		t.Fatal(err)
	}

	inst, err := m.Create(context.Background())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if !regexp.MustCompile(`^vm-[0-9a-f]{20}$`).MatchString(inst.ID) {
		t.Errorf("ID = %q, want vm- plus 20 hex digits", inst.ID)
	}
	if inst.Image != testImage {
		t.Errorf("Image = %q, want the configured %q", inst.Image, testImage)
	}
	other, _ := m.Create(context.Background())
	if other.ID == inst.ID {
		t.Error("two VMs got the same ID")
	}
}

func TestFailedCreateRegistersNothing(t *testing.T) {
	r := newRig(t, fake.WithCapacity(1))
	r.create()

	_, err := r.m.Create(context.Background())

	if !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create = %v, want ErrCapacity", err)
	}
	if got := r.tracked(); got != 1 {
		t.Fatalf("%d sessions tracked, want 1", got)
	}
}

func TestOnlyVMsTheManagerCreatedAreVisible(t *testing.T) {
	r := newRig(t)
	// Created behind the manager's back.
	if _, err := r.fake.Create(context.Background(), vm.Spec{ID: "stranger", Image: testImage, MemoryMB: 64}); err != nil {
		t.Fatal(err)
	}

	if _, err := r.m.Get(context.Background(), "stranger"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get(stranger) = %v, want ErrNotFound", err)
	}
	if _, err := r.m.Console(context.Background(), "stranger"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Console(stranger) = %v, want ErrNotFound", err)
	}
	if err := r.m.Delete(context.Background(), "stranger"); err != nil {
		t.Errorf("Delete(stranger) = %v, want nil", err)
	}
	if !r.vmExists("stranger") {
		t.Error("the manager deleted a VM it did not create")
	}
}

func TestDeleteHidesTheVMAndFreesTheSlot(t *testing.T) {
	r := newRig(t)
	id := r.create()

	if err := r.m.Delete(context.Background(), id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := r.m.Get(context.Background(), id); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if r.fake.Len() != 0 {
		t.Error("the provider still holds the VM")
	}
	if err := r.m.Delete(context.Background(), id); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
}

// A Delete whose caller gave up leaves the VM hidden. Asking again must do the
// work, not return early because the VM is "already being deleted".
func TestDeleteRetryAfterAFailedAttemptDoesTheWork(t *testing.T) {
	r := newRig(t)
	id := r.create()

	if err := r.m.Delete(cancelled(), id); err == nil {
		t.Fatal("Delete with a cancelled context = nil, want its error")
	}
	if _, err := r.m.Get(context.Background(), id); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Get while deletion is pending = %v, want ErrNotFound", err)
	}
	if !r.vmExists(id) {
		t.Fatal("setup: the VM should still exist after a cancelled Delete")
	}

	if err := r.m.Delete(context.Background(), id); err != nil {
		t.Fatalf("retried Delete: %v", err)
	}
	if r.vmExists(id) {
		t.Fatal("the retried Delete returned nil but the VM is still alive")
	}
}

func TestReapFinishesAnAbandonedDelete(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.m.Delete(cancelled(), id)

	r.m.Reap(context.Background())

	if r.vmExists(id) || r.tracked() != 0 {
		t.Fatalf("after Reap: vm exists=%v, tracked=%d; want neither", r.vmExists(id), r.tracked())
	}
}

func TestFinishedVMStaysReadableForTheGracePeriod(t *testing.T) {
	r := newRig(t)
	id := r.create()
	if err := r.fake.Finish(id, vm.StatusFailed, "boom"); err != nil {
		t.Fatal(err)
	}

	r.reapAfter(0) // first seen finished
	r.reapAfter(grace - time.Second)
	got, err := r.m.Get(context.Background(), id)
	if err != nil || got.Status != vm.StatusFailed {
		t.Fatalf("inside the grace period: %+v, %v; want the failed VM still readable", got, err)
	}

	r.reapAfter(time.Second)
	if r.vmExists(id) {
		t.Fatal("the finished VM survived past the grace period")
	}
	if _, err := r.m.Get(context.Background(), id); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Get after removal = %v, want ErrNotFound", err)
	}
}

func TestNeverUsedVMIsRemovedWhenIdle(t *testing.T) {
	r := newRig(t)
	id := r.create()

	r.reapAfter(idle - time.Second)
	if !r.vmExists(id) {
		t.Fatal("removed before the idle timeout")
	}
	r.reapAfter(time.Second)
	if r.vmExists(id) {
		t.Fatal("still alive at the idle timeout")
	}
}

func TestIdleClockRestartsWhenTheBrowserLeaves(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.reapAfter(10 * time.Minute)
	c := r.attach(id)

	r.reapAfter(30 * time.Minute) // t=40m: far past the idle timeout, but attached
	if !r.vmExists(id) {
		t.Fatal("an attached VM was removed as idle")
	}

	c.Close() // idle clock restarts at t=40m
	r.reapAfter(idle - time.Second)
	if !r.vmExists(id) {
		t.Fatal("removed before the idle timeout counted from the disconnect")
	}
	r.reapAfter(time.Second)
	if r.vmExists(id) {
		t.Fatal("still alive after the idle timeout counted from the disconnect")
	}
}

func TestMaxDurationEndsEvenAnAttachedVM(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.attach(id)

	r.reapAfter(maxLife)

	if r.vmExists(id) {
		t.Fatal("an attached VM outlived the maximum duration")
	}
}

func TestFailedSecondAttachDoesNotMarkTheVMIdle(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.attach(id)

	if _, err := r.m.Console(context.Background(), id); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Fatalf("second Console = %v, want ErrAlreadyAttached", err)
	}

	r.reapAfter(idle + time.Minute)
	if !r.vmExists(id) {
		t.Fatal("the failed second attach cleared the first browser's in-use mark")
	}
}

func TestClosingAConsoleTwiceCountsOnce(t *testing.T) {
	r := newRig(t)
	id := r.create()
	first := r.attach(id)
	first.Close()
	second := r.attach(id)
	defer second.Close()

	first.Close() // must not release the second browser's reservation

	r.reapAfter(idle + time.Minute)
	if !r.vmExists(id) {
		t.Fatal("a repeated Close released someone else's attachment")
	}
}

func TestReapForgetsSessionsTheProviderLost(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.fake.Delete(context.Background(), id) // gone behind the manager's back

	r.m.Reap(context.Background())

	if got := r.tracked(); got != 0 {
		t.Fatalf("%d sessions still tracked, want 0", got)
	}
}

// gated pauses Console after the provider has been asked, so a test can run
// Reap in the gap the reservation is meant to protect.
type gated struct {
	vm.Provider
	entered chan struct{}
	proceed chan struct{}
}

func (g *gated) Console(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	g.entered <- struct{}{}
	<-g.proceed
	return g.Provider.Console(ctx, id)
}

func TestReapCannotRemoveAVMWhoseAttachIsInFlight(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	g := &gated{Provider: f, entered: make(chan struct{}, 1), proceed: make(chan struct{})}
	r := newRigOver(t, f, g)
	id := r.create()

	type result struct {
		c   io.ReadWriteCloser
		err error
	}
	done := make(chan result, 1)
	go func() {
		c, err := r.m.Console(context.Background(), id)
		done <- result{c, err}
	}()
	<-g.entered // the provider is being asked; the session is not yet marked attached by the provider

	r.reapAfter(idle + time.Minute) // idle by the clock, but an attach is in flight
	if !r.vmExists(id) {
		t.Fatal("Reap removed the VM while an attach was in flight")
	}

	close(g.proceed)
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("Console: %v", res.err)
		}
		res.c.Close()
	case <-time.After(timeout):
		t.Fatal("Console did not return")
	}
}

func TestConcurrentUse(t *testing.T) {
	r := newRig(t)
	r.m.now = time.Now // real clock: this test is about races, not timing

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				inst, err := r.m.Create(context.Background())
				if err != nil {
					continue
				}
				if c, err := r.m.Console(context.Background(), inst.ID); err == nil {
					c.Close()
				}
				r.m.Get(context.Background(), inst.ID)
				r.m.Reap(context.Background())
				r.m.Delete(context.Background(), inst.ID)
			}
		}()
	}
	wg.Wait()

	r.m.Reap(context.Background())
	if r.fake.Len() != 0 || r.tracked() != 0 {
		t.Fatalf("leftovers: provider %d, tracked %d", r.fake.Len(), r.tracked())
	}
}

func TestRunReapsOnATickAndStopsWithItsContext(t *testing.T) {
	r := newRig(t)
	id := r.create()
	r.clk.Advance(idle + time.Minute)

	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		r.m.Run(ctx, 5*time.Millisecond)
		close(stopped)
	}()

	deadline := time.Now().Add(timeout)
	for r.vmExists(id) {
		if time.Now().After(deadline) {
			t.Fatal("Run never removed the idle VM")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case <-stopped:
	case <-time.After(timeout):
		t.Fatal("Run did not stop when its context ended")
	}
}
