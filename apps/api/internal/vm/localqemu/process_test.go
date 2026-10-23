package localqemu

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// TestHelperProcess is not a test. The tests below run the test binary again
// as a stand-in for QEMU, selected by FAKE_QEMU.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv("FAKE_QEMU")
	if mode == "" {
		return // an ordinary test run
	}
	out := os.Stdout
	switch mode {
	case "boot": // prints a banner, then runs until killed
		_, _ = out.WriteString("booted\n")
		time.Sleep(time.Hour)
	case "echo": // a guest that echoes the keyboard, like a shell would
		_, _ = out.WriteString("ready\n")
		_, _ = io.Copy(out, os.Stdin)
		os.Exit(0)
	case "silent": // alive but never prints
		time.Sleep(time.Hour)
	case "slowboot": // prints after FAKE_DELAY_MS, then runs until killed
		ms, _ := strconv.Atoi(os.Getenv("FAKE_DELAY_MS"))
		time.Sleep(time.Duration(ms) * time.Millisecond)
		_, _ = out.WriteString("up\n")
		time.Sleep(time.Hour)
	case "crash": // QEMU refusing to start
		os.Stderr.WriteString("qemu-system-x86_64: could not open image\n")
		os.Exit(1)
	case "poweroff": // guest shuts down by itself
		_, _ = out.WriteString("bye\n")
		os.Exit(0)
	case "child": // leaves a grandchild behind, like a helper QEMU might
		c := exec.Command("sleep", "300") //nolint:noctx // the point is a grandchild that outlives this process
		if err := c.Start(); err != nil {
			os.Exit(3)
		}
		fmt.Fprintln(out, c.Process.Pid)
		time.Sleep(time.Hour)
	case "flood": // 16 MB of output that nobody reads
		chunk := make([]byte, 64<<10)
		for i := 0; i < 256; i++ {
			_, _ = out.Write(chunk)
		}
		os.Exit(0)
	}
	os.Exit(2)
}

func fakeCommand(mode string, env ...string) commandFunc {
	return func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestHelperProcess$") //nolint:gosec // G204: re-runs this test binary
		// atexit_sleep_ms=0: a race-enabled binary otherwise sleeps 1s at exit.
		cmd.Env = append(append(os.Environ(), "FAKE_QEMU="+mode, "GORACE=atexit_sleep_ms=0"), env...)
		return cmd
	}
}

func startFakeIn(t *testing.T, root context.Context, mode string, timeout time.Duration, env ...string) *process {
	t.Helper()
	p := newProcess(root, fakeCommand(mode, env...), "ignored", okSpec(), nil, time.Now())
	if err := p.start(timeout); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(p.stop)
	return p
}

func startFake(t *testing.T, mode string, timeout time.Duration, env ...string) *process {
	t.Helper()
	root, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return startFakeIn(t, root, mode, timeout, env...)
}

func waitClosed(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

func waitStatus(t *testing.T, p *process, want vm.Status) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if p.snapshot().Status == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("status = %s, want %s", p.snapshot().Status, want)
}

const patient = 30 * time.Second // a start timeout that must never fire

func TestProcessBootsThenStops(t *testing.T) {
	p := startFake(t, "boot", patient)
	waitStatus(t, p, vm.StatusRunning)

	r := mustAttach(t, p.out)
	buf := make([]byte, 16)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "booted\n" {
		t.Fatalf("console = %q, %v", buf[:n], err)
	}

	p.stop()
	waitClosed(t, p.done, "done")
	if got := p.snapshot(); got.Status != vm.StatusStopped || got.Reason != "" {
		t.Fatalf("after stop: %+v", got)
	}
	if got := p.snapshot(); got.ID != "vm-1" || got.Image != "/images/os.iso" || got.CreatedAt.IsZero() {
		t.Fatalf("snapshot lost identity: %+v", got)
	}
	if _, err := r.Read(buf); err != io.EOF { //nolint:errorlint // io.Reader contract: Read returns io.EOF itself, unwrapped
		t.Fatalf("console after stop = %v, want EOF", err)
	}
	p.stop() // again: no panic, no hang
}

func TestProcessCrashIsFailedWithQEMUsOwnWords(t *testing.T) {
	p := startFake(t, "crash", patient)
	r := mustAttach(t, p.out)

	// Reading to EOF and THEN looking at the status is the contract: EOF
	// means the final status is already recorded.
	if out, err := io.ReadAll(r); err != nil || len(out) != 0 {
		t.Fatalf("console = %q, %v", out, err)
	}
	got := p.snapshot()
	if got.Status != vm.StatusFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	for _, want := range []string{"exit status 1", "could not open image"} {
		if !strings.Contains(got.Reason, want) {
			t.Errorf("reason %q does not mention %q", got.Reason, want)
		}
	}
}

func TestProcessGuestPoweringOffIsStoppedNotFailed(t *testing.T) {
	// Exiting right after its first byte races the start watcher; the end
	// state must be right every time.
	for i := 0; i < 25; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			p := startFake(t, "poweroff", patient)
			out, err := io.ReadAll(mustAttach(t, p.out))
			if err != nil || string(out) != "bye\n" {
				t.Fatalf("console = %q, %v", out, err)
			}
			if got := p.snapshot(); got.Status != vm.StatusStopped || got.Reason != "" {
				t.Fatalf("got %+v, want stopped with no reason", got)
			}
			time.Sleep(time.Millisecond) // give a late watcher the chance to misbehave
			if got := p.snapshot().Status; got != vm.StatusStopped {
				t.Fatalf("status changed after the end: %s", got)
			}
		})
	}
}

func TestProcessSilentVMIsFailedAndKilled(t *testing.T) {
	p := startFake(t, "silent", 150*time.Millisecond)
	waitClosed(t, p.done, "the silent VM to be killed")

	got := p.snapshot()
	if got.Status != vm.StatusFailed || !strings.Contains(got.Reason, "no console output within 150ms") {
		t.Fatalf("got %+v", got)
	}
}

func TestProcessFailedToStart(t *testing.T) {
	cmdf := func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "/nonexistent/qemu-binary")
	}
	p := newProcess(context.Background(), cmdf, "x", okSpec(), nil, time.Now())
	if err := p.start(patient); err == nil {
		t.Fatal("start succeeded for a missing binary")
	}

	waitClosed(t, p.done, "done after a failed start")
	if got := p.snapshot(); got.Status != vm.StatusFailed || !strings.Contains(got.Reason, "start qemu") {
		t.Fatalf("got %+v", got)
	}
	p.stop() // must not hang
	if out, err := io.ReadAll(mustAttach(t, p.out)); err != nil || len(out) != 0 {
		t.Fatalf("console = %q, %v", out, err)
	}
}

func TestProcessStopFromManyGoroutines(t *testing.T) {
	p := startFake(t, "boot", patient)
	waitStatus(t, p, vm.StatusRunning)

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); p.stop() }()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "concurrent stops")
	if got := p.snapshot().Status; got != vm.StatusStopped {
		t.Fatalf("status = %s", got)
	}
}

func TestProcessStopKillsTheWholeGroup(t *testing.T) {
	if _, err := os.Stat("/proc/self/stat"); err != nil {
		t.Skip("needs /proc")
	}
	p := startFake(t, "child", patient)
	waitStatus(t, p, vm.StatusRunning)

	line := make([]byte, 32)
	n, err := mustAttach(t, p.out).Read(line)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(line[:n])))
	if err != nil {
		t.Fatalf("pid line %q: %v", line[:n], err)
	}
	if st := procState(pid); st == "" || st == "Z" {
		t.Fatalf("grandchild not running before stop (state %q)", st)
	}

	p.stop()

	// A killed grandchild is reaped by init; until then it shows as Z.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if st := procState(pid); st == "" || st == "Z" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("grandchild %d survived stop (state %q)", pid, procState(pid))
}

// The sink exists for this: nobody reads, QEMU must still finish.
func TestProcessOutputNobodyReadsDoesNotBlockQEMU(t *testing.T) {
	p := startFake(t, "flood", patient)
	waitClosed(t, p.done, "a 16 MB flood to finish with no reader")

	if got := p.snapshot().Status; got != vm.StatusStopped {
		t.Fatalf("status = %s, want stopped", got)
	}
	if p.out.droppedBytes() == 0 {
		t.Fatal("nothing dropped from 16 MB into a 64 KB queue")
	}
	out, err := io.ReadAll(mustAttach(t, p.out))
	if err != nil || len(out) != sinkSize {
		t.Fatalf("kept %d bytes (%v), want exactly %d", len(out), err, sinkSize)
	}
}

func TestProcessProviderShutdownKillsIt(t *testing.T) {
	root, closeProvider := context.WithCancel(context.Background())
	defer closeProvider()
	p := startFakeIn(t, root, "boot", patient)
	waitStatus(t, p, vm.StatusRunning)

	closeProvider() // what Provider.Close does
	waitClosed(t, p.done, "the VM to die with its provider")
	if got := p.snapshot().Status; got != vm.StatusStopped {
		t.Fatalf("status = %s, want stopped", got)
	}
}

// Whichever of "first byte" and "timeout" wins, the process ends in exactly
// one coherent final state.
func TestProcessStartTimeoutRace(t *testing.T) {
	for i := 0; i < 20; i++ {
		delay := fmt.Sprint((i * 3) % 40) // 0..39 ms against a 20 ms timeout
		p := startFake(t, "slowboot", 20*time.Millisecond, "FAKE_DELAY_MS="+delay)
		time.Sleep(60 * time.Millisecond)
		p.stop()

		got := p.snapshot()
		switch got.Status {
		case vm.StatusStopped:
			if got.Reason != "" {
				t.Fatalf("delay %s: stopped with reason %q", delay, got.Reason)
			}
		case vm.StatusFailed:
			if !strings.Contains(got.Reason, "no console output") {
				t.Fatalf("delay %s: failed for the wrong reason: %q", delay, got.Reason)
			}
		default:
			t.Fatalf("delay %s: ended as %s", delay, got.Status)
		}
	}
}

func TestProcessLeavesNoGoroutinesBehind(t *testing.T) {
	time.Sleep(100 * time.Millisecond) // let earlier tests' goroutines unwind
	before := runtime.NumGoroutine()

	// Includes the case the watcher must handle by itself: a process that
	// ends long before its start timeout.
	for _, mode := range []string{"boot", "poweroff", "crash"} {
		p := startFake(t, mode, time.Hour)
		p.stop()
		waitClosed(t, p.done, mode)
	}

	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := runtime.NumGoroutine(); after > before {
		buf := make([]byte, 1<<16)
		t.Fatalf("goroutines: %d before, %d after\n%s", before, after, buf[:runtime.Stack(buf, true)])
	}
}

// procState returns the /proc state letter, or "" if the process is gone.
// A zombie ("Z") is dead but not yet reaped by its new parent.
func procState(pid int) string {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return ""
	}
	s := string(b)
	return strings.Fields(s[strings.LastIndex(s, ")")+1:])[0]
}

// A process that was never started, for testing state rules directly.
func newIdle(t *testing.T) *process {
	t.Helper()
	p := newProcess(context.Background(), fakeCommand("boot"), "ignored", okSpec(), nil, time.Now())
	t.Cleanup(p.cancel)
	return p
}

// The end state is final. The start watcher can run late, after the waiter
// has finished, and must not undo it.
func TestFinalStatusIsFinal(t *testing.T) {
	p := newIdle(t)
	if !p.settle(vm.StatusStopped, "") {
		t.Fatal("first settle refused")
	}
	if p.settle(vm.StatusFailed, "too late") {
		t.Error("a second settle was accepted")
	}
	p.markRunning() // a late watcher
	if got := p.snapshot(); got.Status != vm.StatusStopped || got.Reason != "" {
		t.Fatalf("final state was changed: %+v", got)
	}
}

func TestMarkRunningOnlyMovesStarting(t *testing.T) {
	p := newIdle(t)
	if got := p.snapshot().Status; got != vm.StatusStarting {
		t.Fatalf("new process is %s, want starting", got)
	}
	p.markRunning()
	if got := p.snapshot().Status; got != vm.StatusRunning {
		t.Fatalf("status = %s, want running", got)
	}
}

// With a byte already in and the timer already expired, both branches of the
// watcher's select are ready. The byte must win every time.
func TestWatchStartPrefersAByteOverATimeout(t *testing.T) {
	for i := 0; i < 300; i++ {
		p := newIdle(t)
		p.out.Write([]byte("x"))
		p.watchStart(time.Nanosecond)
		if got := p.snapshot().Status; got != vm.StatusRunning {
			t.Fatalf("iteration %d: status = %s (%q), want running", i, got, p.snapshot().Reason)
		}
	}
}

// A process that ends by itself must release its context. Otherwise every
// VM that ever exited stays registered with the provider's context until
// Close.
func TestProcessReleasesItsContextWhenItEnds(t *testing.T) {
	p := startFake(t, "poweroff", patient)
	waitClosed(t, p.done, "done")
	if p.ctx.Err() == nil {
		t.Fatal("the process's context is still live after it ended")
	}
}

// abort ends a process that never ran, and releases anyone waiting on it.
func TestProcessAbortReleasesWaiters(t *testing.T) {
	p := newIdle(t)
	released := make(chan struct{})
	go func() { <-p.done; close(released) }()

	p.abort("changed my mind")
	waitClosed(t, released, "a waiter on a process that was aborted")
	if got := p.snapshot(); got.Status != vm.StatusFailed || got.Reason != "changed my mind" {
		t.Fatalf("got %+v", got)
	}
	if out, err := io.ReadAll(mustAttach(t, p.out)); err != nil || len(out) != 0 {
		t.Fatalf("console of an aborted process = %q, %v", out, err)
	}
}
