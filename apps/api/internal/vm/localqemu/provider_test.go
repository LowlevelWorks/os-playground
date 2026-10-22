package localqemu

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/vmtest"
)

// testImage makes a file that stands in for the OS image. The fake QEMU never
// reads it; the provider only checks that it exists.
func testImage(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "img")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "os.iso")
	if err := os.WriteFile(path, []byte("not really an iso"), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func specFor(image, id string) vm.Spec { return vm.Spec{ID: id, Image: image, MemoryMB: 128} }

// newTestProvider returns a provider whose "QEMU" is the fake guest in the
// given mode. It is closed, and every process reaped, when the test ends.
func newTestProvider(t *testing.T, mode string, tweak ...func(*Config)) *Provider {
	t.Helper()
	cfg := Config{QEMUBin: os.Args[0], StartTimeout: patient}
	for _, f := range tweak {
		f(&cfg)
	}
	p, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	p.command = fakeCommand(mode)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p
}

func bg() context.Context { return context.Background() }

func (p *Provider) waitFinal(t *testing.T, id string) vm.Instance {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		inst, err := p.Get(bg(), id)
		if err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
		if inst.Status.IsTerminal() {
			return inst
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%q never reached a final status", id)
	return vm.Instance{}
}

// len reports how many slots are in use, including VMs being deleted.
func (p *Provider) len() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.vms)
}

func (p *Provider) procOf(t *testing.T, id string) *process {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.vms[id]
	if !ok {
		t.Fatalf("no entry for %q", id)
	}
	return e.proc
}

// The same behavioural contract the in-memory fake passes, against the real
// provider code with a fake QEMU underneath.
func TestProviderContract(t *testing.T) {
	img := testImage(t)
	vmtest.Run(t, vmtest.Factory{
		New: func(t *testing.T) vm.Provider {
			return newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 3 })
		},
		Spec:       func(id string) vm.Spec { return specFor(img, id) },
		Capacity:   3,
		RunTimeout: 20 * time.Second,
	})
}

func TestNewChecksTheConfiguration(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{"too many CPUs", Config{QEMUBin: os.Args[0], Options: Options{CPUs: 99}}, "CPUs=99"},
		{"unknown accelerator", Config{QEMUBin: os.Args[0], Options: Options{Accelerator: "kvn"}}, "kvn"},
		{"negative MaxVMs", Config{QEMUBin: os.Args[0], MaxVMs: -1}, "MaxVMs=-1"},
		{"negative StartTimeout", Config{QEMUBin: os.Args[0], StartTimeout: -time.Second}, "StartTimeout"},
		{"QEMU missing", Config{QEMUBin: "/nonexistent/qemu-system-x86_64"}, "QEMU not found"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(tc.cfg)
			if err == nil {
				p.Close(bg())
				t.Fatal("New accepted the configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestNewReportsEveryProblemAtOnce(t *testing.T) {
	_, err := New(Config{QEMUBin: os.Args[0], Options: Options{CPUs: 99}, MaxVMs: -2, StartTimeout: -1})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"CPUs=99", "MaxVMs=-2", "StartTimeout"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	p, err := New(Config{QEMUBin: os.Args[0]})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close(bg())
	if p.startTimeout != defaultStartTimeout {
		t.Errorf("startTimeout = %s", p.startTimeout)
	}
	if p.opts.CPUs != defaultCPUs || p.opts.MaxMemoryMB != defaultMaxMemoryMB {
		t.Errorf("options not normalised: %+v", p.opts)
	}
	if !filepath.IsAbs(p.qemuBin) {
		t.Errorf("qemuBin %q was not resolved to a path", p.qemuBin)
	}
}

func TestCreateRejectsBadImagesAndLeavesNothingBehind(t *testing.T) {
	p := newTestProvider(t, "echo")
	dir := t.TempDir()
	tests := map[string]string{
		"missing file": filepath.Join(dir, "absent.iso"),
		"a directory":  dir,
		"relative":     "os.iso",
	}
	for name, image := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := p.Create(bg(), specFor(image, "vm-x"))
			if !errors.Is(err, vm.ErrInvalidSpec) {
				t.Fatalf("err = %v, want ErrInvalidSpec", err)
			}
			if p.len() != 0 {
				t.Fatalf("%d slots in use after a rejected Create", p.len())
			}
		})
	}
}

func TestCreateLaunchesQEMUWithTheBuiltArguments(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")

	var gotName string
	var gotArgs []string
	inner := p.command
	p.command = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName, gotArgs = name, args
		return inner(ctx, name, args...)
	}

	spec := specFor(img, "vm-args")
	if _, err := p.Create(bg(), spec); err != nil {
		t.Fatal(err)
	}
	want, err := buildArgs(spec, p.opts)
	if err != nil {
		t.Fatal(err)
	}
	if gotName != p.qemuBin {
		t.Errorf("ran %q, want the resolved binary %q", gotName, p.qemuBin)
	}
	if strings.Join(gotArgs, "\x00") != strings.Join(want, "\x00") {
		t.Errorf("args = %q, want %q", gotArgs, want)
	}
	if !hasPair(gotArgs, "-nic", "none") || !hasPair(gotArgs, "-name", "vm-args") {
		t.Errorf("security or identity flags missing: %q", gotArgs)
	}
}

func TestCreateReportsWhatWasCreated(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")
	fixed := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return fixed }

	inst, err := p.Create(bg(), specFor(img, "vm-info"))
	if err != nil {
		t.Fatal(err)
	}
	if inst.ID != "vm-info" || inst.Image != img || !inst.CreatedAt.Equal(fixed) {
		t.Fatalf("got %+v", inst)
	}
	if inst.Status != vm.StatusStarting && inst.Status != vm.StatusRunning {
		t.Fatalf("status = %s", inst.Status)
	}
}

func TestCreateStartFailureLeavesNoSlotBehind(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 1 })
	working := p.command
	p.command = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/qemu")
	}

	_, err := p.Create(bg(), specFor(img, "vm-bad"))
	if err == nil || errors.Is(err, vm.ErrInvalidSpec) {
		t.Fatalf("err = %v, want a start failure that is not the caller's fault", err)
	}
	if p.len() != 0 {
		t.Fatal("a failed start kept its slot")
	}
	if _, err := p.Get(bg(), "vm-bad"); !errors.Is(err, vm.ErrNotFound) {
		t.Fatalf("Get = %v, want ErrNotFound", err)
	}

	p.command = working // with the only slot free again, a good start works
	if _, err := p.Create(bg(), specFor(img, "vm-good")); err != nil {
		t.Fatalf("Create after a failed start: %v", err)
	}
}

// The caller's context can expire after the slot is taken but before QEMU is
// launched. Nothing may be left behind, and a waiting Delete must not hang.
func TestCreateCancelledInTheMiddleLeavesNothing(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")
	ctx, cancel := context.WithCancel(bg())
	inner := p.command
	p.command = func(c context.Context, name string, args ...string) *exec.Cmd {
		cancel() // runs while Create builds the process, after its first ctx check
		return inner(c, name, args...)
	}

	if _, err := p.Create(ctx, specFor(img, "vm-mid")); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if p.len() != 0 {
		t.Fatal("a cancelled Create kept its slot")
	}
	if err := p.Delete(bg(), "vm-mid"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestStartTimeoutIsHonoured(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "silent", func(c *Config) { c.StartTimeout = 150 * time.Millisecond })
	if _, err := p.Create(bg(), specFor(img, "vm-quiet")); err != nil {
		t.Fatal(err)
	}
	got := p.waitFinal(t, "vm-quiet")
	if got.Status != vm.StatusFailed || !strings.Contains(got.Reason, "no console output within 150ms") {
		t.Fatalf("got %+v", got)
	}
}

func TestFailedVMReportsWhyAndRefusesConsole(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "crash")
	if _, err := p.Create(bg(), specFor(img, "vm-crash")); err != nil {
		t.Fatal(err)
	}
	got := p.waitFinal(t, "vm-crash")
	if got.Status != vm.StatusFailed || !strings.Contains(got.Reason, "could not open image") {
		t.Fatalf("got %+v", got)
	}
	if _, err := p.Console(bg(), "vm-crash"); !errors.Is(err, vm.ErrNotRunning) {
		t.Fatalf("Console = %v, want ErrNotRunning", err)
	}
}

// A finished VM keeps its slot until Delete, so the caller can read how it
// ended before the space is reused.
func TestFinishedVMHoldsItsSlotUntilDeleted(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "poweroff", func(c *Config) { c.MaxVMs = 1 })
	if _, err := p.Create(bg(), specFor(img, "vm-one")); err != nil {
		t.Fatal(err)
	}
	if got := p.waitFinal(t, "vm-one"); got.Status != vm.StatusStopped {
		t.Fatalf("got %+v", got)
	}
	if _, err := p.Create(bg(), specFor(img, "vm-two")); !errors.Is(err, vm.ErrCapacity) {
		t.Fatalf("Create beside a finished VM = %v, want ErrCapacity", err)
	}
	if err := p.Delete(bg(), "vm-one"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(bg(), specFor(img, "vm-two")); err != nil {
		t.Fatalf("Create after Delete: %v", err)
	}
}

// Only MaxVMs may succeed, however many callers race for the slots.
func TestCapacityHoldsUnderConcurrentCreates(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 2 })

	const callers = 10
	var ok, full atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, err := p.Create(bg(), specFor(img, fmt.Sprintf("vm-c%d", i)))
			switch {
			case err == nil:
				ok.Add(1)
			case errors.Is(err, vm.ErrCapacity):
				full.Add(1)
			default:
				t.Errorf("Create: %v", err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if ok.Load() != 2 || full.Load() != callers-2 {
		t.Fatalf("%d succeeded and %d were refused, want 2 and %d", ok.Load(), full.Load(), callers-2)
	}
	if p.len() != 2 {
		t.Fatalf("%d slots in use, want 2", p.len())
	}
}

// When Delete returns, the process is gone, for every caller of a concurrent
// Delete and not only the first.
func TestDeleteReturnsOnlyOnceTheProcessIsDead(t *testing.T) {
	img := testImage(t)
	for round := 0; round < 8; round++ {
		p := newTestProvider(t, "echo")
		id := fmt.Sprintf("vm-d%d", round)
		if _, err := p.Create(bg(), specFor(img, id)); err != nil {
			t.Fatal(err)
		}
		proc := p.procOf(t, id)

		var wg sync.WaitGroup
		for i := 0; i < 6; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if err := p.Delete(bg(), id); err != nil {
					t.Errorf("Delete: %v", err)
				}
				if !proc.ended() {
					t.Error("Delete returned while the process was still alive")
				}
			}()
		}
		wg.Wait()
		if p.len() != 0 {
			t.Fatalf("%d slots in use after Delete", p.len())
		}
	}
}

// Once Delete has begun the VM is gone to callers, though its slot is held
// until the process is dead.
func TestDeletingVMIsInvisibleButStillHoldsItsSlot(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 1 })
	if _, err := p.Create(bg(), specFor(img, "vm-dying")); err != nil {
		t.Fatal(err)
	}
	p.mu.Lock()
	p.vms["vm-dying"].deleting = true // as Delete does, before the process is dead
	p.mu.Unlock()

	if _, err := p.Get(bg(), "vm-dying"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Get = %v, want ErrNotFound", err)
	}
	if _, err := p.Console(bg(), "vm-dying"); !errors.Is(err, vm.ErrNotFound) {
		t.Errorf("Console = %v, want ErrNotFound", err)
	}
	if _, err := p.Create(bg(), specFor(img, "vm-new")); !errors.Is(err, vm.ErrCapacity) {
		t.Errorf("Create = %v, want ErrCapacity: the slot is still in use", err)
	}
	if err := p.Delete(bg(), "vm-dying"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Create(bg(), specFor(img, "vm-new")); err != nil {
		t.Fatalf("Create once the process is dead: %v", err)
	}
}

func TestDeleteWithAnExpiredContextChangesNothing(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")
	if _, err := p.Create(bg(), specFor(img, "vm-slow")); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(bg())
	cancel()
	if err := p.Delete(ctx, "vm-slow"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Delete = %v, want context.Canceled", err)
	}
	// A context that is already dead changes nothing, as the contract says.
	if _, err := p.Get(bg(), "vm-slow"); err != nil {
		t.Fatalf("a cancelled Delete hid the VM: %v", err)
	}
}

func TestCloseKillsEveryVMAndWaitsForThem(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 3 })
	var procs []*process
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("vm-k%d", i)
		if _, err := p.Create(bg(), specFor(img, id)); err != nil {
			t.Fatal(err)
		}
		procs = append(procs, p.procOf(t, id))
	}

	if err := p.Close(bg()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, proc := range procs {
		if !proc.ended() {
			t.Errorf("vm %d still alive after Close returned", i)
		}
		if got := proc.snapshot().Status; got != vm.StatusStopped {
			t.Errorf("vm %d is %s, want stopped", i, got)
		}
	}
	if _, err := p.Create(bg(), specFor(img, "vm-late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Create after Close = %v, want ErrClosed", err)
	}
	if err := p.Close(bg()); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

func TestCloseGivesUpWhenItsContextExpires(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")
	if _, err := p.Create(bg(), specFor(img, "vm-z")); err != nil {
		t.Fatal(err)
	}
	proc := p.procOf(t, "vm-z")

	ctx, cancel := context.WithCancel(bg())
	cancel()
	if err := p.Close(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Close = %v, want context.Canceled", err)
	}
	// The kill was ordered anyway; the process still dies.
	waitClosed(t, proc.done, "the process to die after an expired Close")
}

// Callers hammering one ID with every operation at once: no panic, no
// deadlock, no slot left over, and the race detector stays quiet.
func TestCreateDeleteGetConsoleRaceOnOneID(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 2 })

	for round := 0; round < 20; round++ {
		var wg sync.WaitGroup
		run := func(f func()) {
			wg.Add(1)
			go func() { defer wg.Done(); f() }()
		}
		for i := 0; i < 2; i++ {
			run(func() { p.Create(bg(), specFor(img, "vm-race")) })
			run(func() { p.Delete(bg(), "vm-race") })
			run(func() { p.Get(bg(), "vm-race") })
			run(func() {
				if c, err := p.Console(bg(), "vm-race"); err == nil {
					c.Write([]byte("x\n"))
					c.Close()
				}
			})
		}
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		waitClosed(t, done, "racing operations (deadlock?)")

		if err := p.Delete(bg(), "vm-race"); err != nil {
			t.Fatal(err)
		}
		if p.len() != 0 {
			t.Fatalf("round %d: %d slots left over", round, p.len())
		}
	}
}

type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestLogsHowAVMEnded(t *testing.T) {
	img := testImage(t)
	var buf lockedBuffer
	p := newTestProvider(t, "crash", func(c *Config) {
		c.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	})
	if _, err := p.Create(bg(), specFor(img, "vm-log")); err != nil {
		t.Fatal(err)
	}
	p.waitFinal(t, "vm-log")

	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "vm ended") && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	out := buf.String()
	for _, want := range []string{"vm started", "vm ended", "status=failed", "could not open image", "id=vm-log"} {
		if !strings.Contains(out, want) {
			t.Errorf("log does not contain %q:\n%s", want, out)
		}
	}
}

// A Delete whose caller gave up leaves the VM hidden and its slot held. The
// slot must come back as soon as the process is dead, with nobody retrying.
func TestDeletionNobodyWaitedForStillFreesTheSlot(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo", func(c *Config) { c.MaxVMs = 1 })
	if _, err := p.Create(bg(), specFor(img, "vm-orphan")); err != nil {
		t.Fatal(err)
	}
	proc := p.procOf(t, "vm-orphan")

	p.mu.Lock()
	p.vms["vm-orphan"].deleting = true // Delete began...
	p.mu.Unlock()
	proc.cancel() // ...ordered the kill, and its caller walked away

	deadline := time.Now().Add(10 * time.Second)
	for p.len() != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if p.len() != 0 {
		t.Fatal("the slot of a deleted VM was never released")
	}
	if _, err := p.Create(bg(), specFor(img, "vm-next")); err != nil {
		t.Fatalf("Create after the background release: %v", err)
	}
}

// A VM that was not being deleted must stay readable after it ends.
func TestFinishedVMStaysReadableUntilDeleted(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "poweroff")
	if _, err := p.Create(bg(), specFor(img, "vm-read")); err != nil {
		t.Fatal(err)
	}
	p.waitFinal(t, "vm-read")
	time.Sleep(50 * time.Millisecond) // time for any background cleanup to misbehave
	if got, err := p.Get(bg(), "vm-read"); err != nil || got.Status != vm.StatusStopped {
		t.Fatalf("Get = %+v, %v; want the stopped VM", got, err)
	}
}

// flipCtx runs fire on its n-th Err call, then reports cancellation. Create
// calls Err once on entry and once after it has reserved its slot, so n=2
// lands exactly between "slot reserved" and "QEMU launched".
type flipCtx struct {
	context.Context
	n, calls int
	fire     func()
}

func (c *flipCtx) Err() error {
	c.calls++
	if c.calls == c.n {
		c.fire()
		return context.Canceled
	}
	return nil
}

// A Delete that arrives while Create has reserved the slot but not launched
// QEMU waits for the process to end. A Create that gives up must end that
// never-started process, or the Delete waits for ever.
func TestCancelledCreateDoesNotLeaveADeleteHanging(t *testing.T) {
	img := testImage(t)
	p := newTestProvider(t, "echo")

	deleted := make(chan error, 1)
	ctx := &flipCtx{Context: bg(), n: 2, fire: func() {
		go func() { deleted <- p.Delete(bg(), "vm-half") }()
		// Wait until the Delete has begun: it has hidden the VM and is about
		// to wait for the process.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			p.mu.Lock()
			begun := p.vms["vm-half"] != nil && p.vms["vm-half"].deleting
			p.mu.Unlock()
			if begun {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}}

	if _, err := p.Create(ctx, specFor(img, "vm-half")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create = %v, want context.Canceled", err)
	}
	select {
	case err := <-deleted:
		if err != nil {
			t.Fatalf("Delete = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Delete hung on a process that was never started")
	}
	if p.len() != 0 {
		t.Fatalf("%d slots left over", p.len())
	}
}
