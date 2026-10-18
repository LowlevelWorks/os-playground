// Package vmtest holds the behavioural contract every vm.Provider must pass.
//
// A provider's own test calls Run with a Factory. The same checks then run
// against the in-memory fake (no environment needed), against local QEMU
// (needs QEMU and an image) and later against KubeVirt (needs a cluster). That
// is how a fake stays trustworthy: it passes exactly what the real thing does.
//
// Every case gets a fresh provider and deletes whatever it created, so a
// failing test does not leak processes from a real provider.
package vmtest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// Factory tells Run how to build the provider under test.
type Factory struct {
	// New returns a fresh provider for one test case. Register any teardown
	// with t.Cleanup.
	New func(t *testing.T) vm.Provider

	// Spec returns a valid spec for the given ID. The image reference differs
	// per provider (a file path for local QEMU, anything for the fake).
	Spec func(id string) vm.Spec

	// Capacity is the number of VMs New's provider allows at once. Zero means
	// no limit and skips the capacity case.
	Capacity int

	// RunTimeout is how long to wait for a VM to reach running. Defaults to
	// 10 seconds; give a real provider longer.
	RunTimeout time.Duration
}

// Run executes the contract.
func Run(t *testing.T, f Factory) {
	t.Helper()
	if f.New == nil || f.Spec == nil {
		t.Fatal("vmtest: Factory.New and Factory.Spec are required")
	}
	if f.RunTimeout <= 0 {
		f.RunTimeout = 10 * time.Second
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) { tc.run(newHarness(t, f)) })
	}
}

var cases = []struct {
	name string
	run  func(*harness)
}{
	{"invalid spec is rejected and creates nothing", invalidSpec},
	{"create returns an instance", createReturnsInstance},
	{"duplicate id is rejected", duplicateID},
	{"get unknown id", getUnknown},
	{"vm reaches running", reachesRunning},
	{"cancelled create creates nothing", cancelledCreate},
	{"cancelled context on other methods", cancelledOtherMethods},
	{"context does not bound the vm lifetime", contextDoesNotBoundLifetime},
	{"console attach and detach", consoleAttachDetach},
	{"console unknown id", consoleUnknown},
	{"delete is idempotent", deleteIdempotent},
	{"delete ends an attached console", deleteEndsConsole},
	{"capacity limit and slot release", capacityLimit},
	{"concurrent creates", concurrentCreates},
	{"concurrent duplicate create", concurrentDuplicateCreate},
	{"concurrent deletes", concurrentDeletes},
}

var idSeq atomic.Uint64

type harness struct {
	t *testing.T
	p vm.Provider
	f Factory
}

func newHarness(t *testing.T, f Factory) *harness {
	t.Helper()
	p := f.New(t)
	if p == nil {
		t.Fatal("vmtest: Factory.New returned nil")
	}
	return &harness{t: t, p: p, f: f}
}

// ctx returns a context that is cancelled when the test ends.
func (h *harness) ctx() context.Context {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	h.t.Cleanup(cancel)
	return ctx
}

// newID returns a valid, unique VM name.
func (h *harness) newID() string { return fmt.Sprintf("ct-%d", idSeq.Add(1)) }

// cleanup deletes id when the test ends. Delete of a missing VM is nil by
// contract, so registering it for an ID that was never created is harmless.
func (h *harness) cleanup(id string) {
	h.t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := h.p.Delete(ctx, id); err != nil {
			h.t.Errorf("cleanup: Delete(%q): %v", id, err)
		}
	})
}

// create makes a VM and arranges for it to be deleted.
func (h *harness) create() vm.Instance {
	h.t.Helper()
	id := h.newID()
	inst, err := h.p.Create(h.ctx(), h.f.Spec(id))
	if err != nil {
		h.t.Fatalf("Create(%q): %v", id, err)
	}
	h.cleanup(id)
	return inst
}

// waitRunning polls Get until the VM is running, never sleeping a fixed time.
func (h *harness) waitRunning(id string) vm.Instance {
	h.t.Helper()
	ctx := h.ctx()
	deadline := time.Now().Add(h.f.RunTimeout)
	for {
		inst, err := h.p.Get(ctx, id)
		if err != nil {
			h.t.Fatalf("Get(%q) while waiting for running: %v", id, err)
		}
		if inst.Status == vm.StatusRunning {
			return inst
		}
		if inst.Status.IsTerminal() {
			h.t.Fatalf("%q reached %s (%q) before running", id, inst.Status, inst.Reason)
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("%q still %s after %s", id, inst.Status, h.f.RunTimeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func invalidSpec(h *harness) {
	t := h.t
	id := h.newID()
	h.cleanup(id)
	spec := h.f.Spec(id)
	spec.MemoryMB = 0

	if _, err := h.p.Create(h.ctx(), spec); !errors.Is(err, vm.ErrInvalidSpec) {
		t.Fatalf("Create(invalid) = %v, want ErrInvalidSpec", err)
	}
	if _, err := h.p.Get(h.ctx(), id); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after rejected Create = %v, want ErrNotFound", err)
	}
}

func createReturnsInstance(h *harness) {
	t := h.t
	id := h.newID()
	h.cleanup(id)
	inst, err := h.p.Create(h.ctx(), h.f.Spec(id))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if inst.ID != id {
		t.Errorf("ID = %q, want %q", inst.ID, id)
	}
	switch inst.Status {
	case vm.StatusProvisioning, vm.StatusStarting, vm.StatusRunning:
	default:
		t.Errorf("Status = %q, want provisioning, starting or running", inst.Status)
	}
	if inst.Image == "" {
		t.Error("Image is empty; it must report the reference actually used")
	}
	if inst.CreatedAt.IsZero() {
		t.Error("CreatedAt is zero")
	}
}

func duplicateID(h *harness) {
	t := h.t
	first := h.create()

	_, err := h.p.Create(h.ctx(), h.f.Spec(first.ID))
	if !errors.Is(err, vm.ErrAlreadyExists) {
		t.Fatalf("second Create = %v, want ErrAlreadyExists", err)
	}
	got, err := h.p.Get(h.ctx(), first.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status.IsTerminal() || !got.CreatedAt.Equal(first.CreatedAt) {
		t.Errorf("the original VM was disturbed: %+v (created as %+v)", got, first)
	}
}

func getUnknown(h *harness) {
	if _, err := h.p.Get(h.ctx(), h.newID()); !errors.Is(err, vm.ErrNotFound) {
		h.t.Fatalf("Get(unknown) = %v, want ErrNotFound", err)
	}
}

func reachesRunning(h *harness) {
	inst := h.create()
	got := h.waitRunning(inst.ID)
	if got.ID != inst.ID {
		h.t.Errorf("ID = %q, want %q", got.ID, inst.ID)
	}
}

func cancelledCreate(h *harness) {
	t := h.t
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	id := h.newID()
	h.cleanup(id)

	if _, err := h.p.Create(ctx, h.f.Spec(id)); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create(cancelled ctx) = %v, want context.Canceled", err)
	}
	if _, err := h.p.Get(h.ctx(), id); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("a cancelled Create left a VM behind: Get = %v", err)
	}
}

func cancelledOtherMethods(h *harness) {
	t := h.t
	inst := h.create()
	h.waitRunning(inst.ID)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := h.p.Get(ctx, inst.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("Get(cancelled ctx) = %v, want context.Canceled", err)
	}
	if _, err := h.p.Console(ctx, inst.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("Console(cancelled ctx) = %v, want context.Canceled", err)
	}
	if err := h.p.Delete(ctx, inst.ID); !errors.Is(err, context.Canceled) {
		t.Errorf("Delete(cancelled ctx) = %v, want context.Canceled", err)
	}
	if _, err := h.p.Get(h.ctx(), inst.ID); err != nil {
		t.Errorf("a cancelled call changed the VM: Get = %v", err)
	}
}

// The rule that prevents the worst bug: an HTTP request ending must not kill
// the VM it created.
func contextDoesNotBoundLifetime(h *harness) {
	t := h.t
	ctx, cancel := context.WithCancel(h.ctx())
	id := h.newID()
	h.cleanup(id)

	if _, err := h.p.Create(ctx, h.f.Spec(id)); err != nil {
		t.Fatalf("Create: %v", err)
	}
	cancel()
	h.waitRunning(id)
}

func consoleAttachDetach(h *harness) {
	t := h.t
	inst := h.create()
	h.waitRunning(inst.ID)

	c1, err := h.p.Console(h.ctx(), inst.ID)
	if err != nil {
		t.Fatalf("Console: %v", err)
	}

	// A provider that cannot share the console must say so; one that can may
	// allow a second attachment.
	c2, err := h.p.Console(h.ctx(), inst.ID)
	switch {
	case err == nil:
		c2.Close()
	case errors.Is(err, vm.ErrAlreadyAttached):
	default:
		t.Errorf("second Console = %v, want nil or ErrAlreadyAttached", err)
	}

	c1.Close()

	got, err := h.p.Get(h.ctx(), inst.ID)
	if err != nil || got.Status != vm.StatusRunning {
		t.Fatalf("closing the console changed the VM: %+v, %v", got, err)
	}

	c3, err := h.p.Console(h.ctx(), inst.ID)
	if err != nil {
		t.Fatalf("Console after Close: %v", err)
	}
	c3.Close()
}

func consoleUnknown(h *harness) {
	if _, err := h.p.Console(h.ctx(), h.newID()); !errors.Is(err, vm.ErrNotFound) {
		h.t.Fatalf("Console(unknown) = %v, want ErrNotFound", err)
	}
}

func deleteIdempotent(h *harness) {
	t := h.t
	inst := h.create()

	if err := h.p.Delete(h.ctx(), inst.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := h.p.Get(h.ctx(), inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
	if _, err := h.p.Console(h.ctx(), inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Console after Delete = %v, want ErrNotFound", err)
	}
	if err := h.p.Delete(h.ctx(), inst.ID); err != nil {
		t.Errorf("second Delete = %v, want nil", err)
	}
	if err := h.p.Delete(h.ctx(), h.newID()); err != nil {
		t.Errorf("Delete(unknown) = %v, want nil", err)
	}
}

func deleteEndsConsole(h *harness) {
	t := h.t
	inst := h.create()
	h.waitRunning(inst.ID)

	c, err := h.p.Console(h.ctx(), inst.ID)
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	defer c.Close()

	if err := h.p.Delete(h.ctx(), inst.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := c.Read(buf); err != nil {
				done <- err
				return
			}
		}
	}()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Errorf("read after Delete ended with %v, want io.EOF", err)
		}
	case <-time.After(h.f.RunTimeout):
		t.Fatal("console read did not end after Delete")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Error("write to the console of a deleted VM succeeded")
	}
}

func capacityLimit(h *harness) {
	t := h.t
	n := h.f.Capacity
	if n <= 0 {
		t.Skip("provider has no capacity limit")
	}
	ids := make([]string, 0, n)
	for range n {
		ids = append(ids, h.create().ID)
	}

	if _, err := h.p.Create(h.ctx(), h.f.Spec(h.newID())); !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create beyond capacity = %v, want ErrCapacity", err)
	}
	if err := h.p.Delete(h.ctx(), ids[0]); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if h.create().ID == "" { // fails the test itself if the slot was not freed
		t.Error("empty ID")
	}
}

func concurrentCreates(h *harness) {
	t := h.t
	n := 8
	if c := h.f.Capacity; c > 0 && c < n {
		n = c
	}
	ctx := h.ctx()
	ids := make([]string, n)
	for i := range ids {
		ids[i] = h.newID()
		h.cleanup(ids[i])
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			if _, err := h.p.Create(ctx, h.f.Spec(id)); err != nil {
				t.Errorf("Create(%q): %v", id, err)
			}
		}(id)
	}
	wg.Wait()

	for _, id := range ids {
		if _, err := h.p.Get(ctx, id); err != nil {
			t.Errorf("Get(%q) after concurrent Create: %v", id, err)
		}
	}
}

// Exactly one of several simultaneous creates of the same ID may win.
func concurrentDuplicateCreate(h *harness) {
	t := h.t
	ctx := h.ctx()
	id := h.newID()
	h.cleanup(id)

	const n = 8
	results := make(chan error, n)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, err := h.p.Create(ctx, h.f.Spec(id))
			results <- err
		}()
	}
	close(start)
	wg.Wait()
	close(results)

	won := 0
	for err := range results {
		switch {
		case err == nil:
			won++
		case errors.Is(err, vm.ErrAlreadyExists):
		default:
			t.Errorf("Create = %v, want nil or ErrAlreadyExists", err)
		}
	}
	if won != 1 {
		t.Errorf("%d concurrent Creates of one ID succeeded, want exactly 1", won)
	}
}

func concurrentDeletes(h *harness) {
	t := h.t
	ctx := h.ctx()
	inst := h.create()

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if err := h.p.Delete(ctx, inst.ID); err != nil {
				t.Errorf("Delete = %v, want nil", err)
			}
		})
	}

	wg.Wait()

	if _, err := h.p.Get(ctx, inst.ID); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get after concurrent Deletes = %v, want ErrNotFound", err)
	}
}
