package fake_test

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
	"github.com/os-playground-api/internal/vm/vmtest"
)

var bg = context.Background()

func spec(id string) vm.Spec { return vm.Spec{ID: id, Image: "fake://os", MemoryMB: 128} }

// The fake must pass the same contract as every real provider.
func TestContract(t *testing.T) {
	vmtest.Run(t, vmtest.Factory{
		New: func(t *testing.T) vm.Provider {
			return fake.New(fake.WithAutoRun(), fake.WithCapacity(3))
		},
		Spec:     spec,
		Capacity: 3,
	})
}

func TestLifecycleIsDrivenByHand(t *testing.T) {
	f := fake.New()
	inst, err := f.Create(bg, spec("vm-1"))
	if err != nil || inst.Status != vm.StatusProvisioning {
		t.Fatalf("Create = %+v, %v; want provisioning", inst, err)
	}

	for _, want := range []vm.Status{vm.StatusStarting, vm.StatusRunning} {
		if _, err := f.Console(bg, "vm-1"); !errors.Is(err, vm.ErrNotRunning) {
			t.Fatalf("Console before running = %v, want ErrNotRunning", err)
		}
		if err := f.Advance("vm-1"); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		got, _ := f.Get(bg, "vm-1")
		if got.Status != want {
			t.Fatalf("status = %q, want %q", got.Status, want)
		}
	}

	c, err := f.Console(bg, "vm-1")
	if err != nil {
		t.Fatalf("Console once running: %v", err)
	}
	c.Close()

	if err := f.Advance("vm-1"); err == nil {
		t.Error("Advance past running should fail")
	}
	if err := f.Advance("nope"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Advance(unknown) = %v, want ErrNotFound", err)
	}
}

func TestFinish(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	if _, err := f.Create(bg, spec("vm-1")); err != nil {
		t.Fatal(err)
	}

	if err := f.Finish("vm-1", vm.StatusRunning, ""); err == nil {
		t.Error("Finish with a non-terminal status should fail")
	}
	if err := f.Finish("vm-1", vm.StatusFailed, "kernel panic"); err != nil {
		t.Fatalf("Finish: %v", err)
	}

	got, _ := f.Get(bg, "vm-1")
	if got.Status != vm.StatusFailed || got.Reason != "kernel panic" {
		t.Errorf("Get = %+v, want failed with its reason", got)
	}
	if _, err := f.Console(bg, "vm-1"); !errors.Is(err, vm.ErrNotRunning) {
		t.Errorf("Console on a failed VM = %v, want ErrNotRunning", err)
	}
	if err := f.Finish("vm-1", vm.StatusStopped, ""); err == nil {
		t.Error("a finished VM must not finish again")
	}
	if err := f.Advance("vm-1"); err == nil {
		t.Error("a finished VM must not advance")
	}
	if f.Len() != 1 {
		t.Errorf("Len = %d; a finished VM keeps its slot until Delete", f.Len())
	}
}

func mustCreate(t *testing.T, f *fake.Fake, id string) {
	t.Helper()
	if _, err := f.Create(bg, spec(id)); err != nil {
		t.Fatal(err)
	}
}

func TestConsoleRoundTrip(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	mustCreate(t, f, "vm-1")

	c, err := f.Console(bg, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	g, err := f.Guest("vm-1")
	if err != nil {
		t.Fatal(err)
	}

	// Pipes block until the other side reads, so write from goroutines.
	go func() { _, _ = g.Write([]byte("os> ")) }() // a failed write fails the read below
	buf := make([]byte, 16)
	n, err := c.Read(buf)
	if err != nil || string(buf[:n]) != "os> " {
		t.Fatalf("client read %q, %v; want the guest's prompt", buf[:n], err)
	}

	go func() { _, _ = c.Write([]byte("help\n")) }()
	n, err = g.Read(buf)
	if err != nil || string(buf[:n]) != "help\n" {
		t.Fatalf("guest read %q, %v; want the client's keystrokes", buf[:n], err)
	}
}

func TestFinishEndsConsole(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	mustCreate(t, f, "vm-1")
	c, _ := f.Console(bg, "vm-1")
	defer c.Close()

	if err := f.Finish("vm-1", vm.StatusStopped, "powered off"); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 8)); done <- err }()
	select {
	case err := <-done:
		if !errors.Is(err, io.EOF) {
			t.Errorf("read = %v, want io.EOF", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not end")
	}
	if _, err := c.Write([]byte("x")); err == nil {
		t.Error("write after the VM ended should fail")
	}
}

func TestGuestNeedsAnAttachedConsole(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	mustCreate(t, f, "vm-1")

	if _, err := f.Guest("vm-1"); !errors.Is(err, fake.ErrNotAttached) {
		t.Errorf("Guest with nothing attached = %v, want ErrNotAttached", err)
	}
	if _, err := f.Guest("nope"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Guest(unknown) = %v, want ErrNotFound", err)
	}
}

func TestStaleCloseDoesNotDetachANewConsole(t *testing.T) {
	f := fake.New(fake.WithAutoRun())
	mustCreate(t, f, "vm-1")

	old, _ := f.Console(bg, "vm-1")
	old.Close()
	fresh, err := f.Console(bg, "vm-1")
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()

	old.Close() // closing the old stream again must not free the new attachment
	if _, err := f.Console(bg, "vm-1"); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Errorf("Console = %v, want ErrAlreadyAttached", err)
	}
}

func TestLenTracksSlots(t *testing.T) {
	f := fake.New(fake.WithCapacity(1))
	mustCreate(t, f, "vm-1")
	if f.Len() != 1 {
		t.Fatalf("Len = %d, want 1", f.Len())
	}
	if err := f.Delete(bg, "vm-1"); err != nil {
		t.Fatal(err)
	}
	if f.Len() != 0 {
		t.Fatalf("Len = %d after Delete, want 0", f.Len())
	}
}

func TestClockIsInjectable(t *testing.T) {
	at := time.Date(2026, 10, 18, 9, 0, 0, 0, time.UTC)
	f := fake.New(fake.WithClock(func() time.Time { return at }))
	inst, _ := f.Create(bg, spec("vm-1"))
	if !inst.CreatedAt.Equal(at) {
		t.Errorf("CreatedAt = %v, want %v", inst.CreatedAt, at)
	}
}
