package localqemu

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// startEcho starts a fake guest that echoes whatever is typed, and returns it
// once it is running with its banner not yet read.
func startEcho(t *testing.T) *process {
	t.Helper()
	p := startFake(t, "echo", patient)
	waitStatus(t, p, vm.StatusRunning)
	return p
}

func attachConsole(t *testing.T, p *process) io.ReadWriteCloser {
	t.Helper()
	c, err := p.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	return c
}

// readN reads exactly n bytes or fails the test after a timeout.
func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	done := make(chan error, 1)
	go func() { _, err := io.ReadFull(r, buf); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("read %d bytes: got %q, %v", n, buf, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out reading %d bytes", n)
	}
	return string(buf)
}

func TestConsoleRoundTrip(t *testing.T) {
	c := attachConsole(t, startEcho(t))
	defer c.Close()

	if got := readN(t, c, len("ready\n")); got != "ready\n" {
		t.Fatalf("banner = %q", got)
	}
	if n, err := c.Write([]byte("hello\n")); n != 6 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := readN(t, c, len("hello\n")); got != "hello\n" {
		t.Fatalf("echo = %q", got)
	}
}

func TestConsoleEmptyWriteIsANoOp(t *testing.T) {
	c := attachConsole(t, startEcho(t))
	defer c.Close()
	if n, err := c.Write(nil); n != 0 || err != nil {
		t.Fatalf("Write(nil) = %d, %v", n, err)
	}
}

func TestConsoleAttachNeedsARunningVM(t *testing.T) {
	t.Run("still starting", func(t *testing.T) {
		p := startFake(t, "silent", patient) // never prints, so never runs
		if _, err := p.attach(); !errors.Is(err, vm.ErrNotRunning) {
			t.Fatalf("err = %v, want ErrNotRunning", err)
		}
	})
	t.Run("already finished", func(t *testing.T) {
		p := startFake(t, "poweroff", patient)
		waitClosed(t, p.done, "done")
		if _, err := p.attach(); !errors.Is(err, vm.ErrNotRunning) {
			t.Fatalf("err = %v, want ErrNotRunning", err)
		}
	})
	t.Run("failed to start", func(t *testing.T) {
		p := newProcess(context.Background(), fakeCommand("boot"), "x", okSpec(), nil, time.Now())
		p.cmd.Path = "/nonexistent/qemu"
		_ = p.start(patient)
		if _, err := p.attach(); !errors.Is(err, vm.ErrNotRunning) {
			t.Fatalf("err = %v, want ErrNotRunning", err)
		}
	})
}

func TestConsoleOnlyOneAtATime(t *testing.T) {
	p := startEcho(t)
	c1 := attachConsole(t, p)
	if _, err := p.attach(); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Fatalf("second attach = %v, want ErrAlreadyAttached", err)
	}
	c1.Close()
	c2 := attachConsole(t, p)
	c2.Close()
}

func TestConsoleCloseDetachesButTheVMKeepsRunning(t *testing.T) {
	p := startEcho(t)
	c1 := attachConsole(t, p)
	readN(t, c1, len("ready\n"))
	c1.Close()
	c1.Close() // twice is fine

	if got := p.snapshot().Status; got != vm.StatusRunning {
		t.Fatalf("closing a console changed the VM to %s", got)
	}
	if _, err := c1.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Write after Close = %v, want ErrClosedPipe", err)
	}
	if _, err := c1.Read(make([]byte, 1)); !errors.Is(err, io.ErrClosedPipe) {
		t.Errorf("Read after Close = %v, want ErrClosedPipe", err)
	}

	// The same VM is still alive and answers a new client.
	c2 := attachConsole(t, p)
	defer c2.Close()
	c2.Write([]byte("again\n"))
	if got := readN(t, c2, len("again\n")); got != "again\n" {
		t.Fatalf("echo after reattach = %q", got)
	}
}

func TestConsoleStaleClientCannotTypeIntoANewSession(t *testing.T) {
	p := startEcho(t)
	old := attachConsole(t, p)
	old.Close()
	c := attachConsole(t, p)
	defer c.Close()
	readN(t, c, len("ready\n"))

	if _, err := old.Write([]byte("ghost\n")); err == nil {
		t.Fatal("a closed console could still write")
	}
	c.Write([]byte("real\n"))
	if got := readN(t, c, len("real\n")); got != "real\n" {
		t.Fatalf("new client saw %q, want only its own input", got)
	}
}

func TestConsoleReattachFindsOutputProducedWhileAway(t *testing.T) {
	p := startEcho(t)
	c1 := attachConsole(t, p)
	readN(t, c1, len("ready\n"))
	c1.Close()

	// Typed by nobody attached: written straight to the process.
	if _, err := p.writeStdin([]byte("while away\n")); err != nil {
		t.Fatal(err)
	}
	c2 := attachConsole(t, p)
	defer c2.Close()
	if got := readN(t, c2, len("while away\n")); got != "while away\n" {
		t.Fatalf("got %q", got)
	}
}

func TestConsoleVMEndingEndsTheStream(t *testing.T) {
	p := startEcho(t)
	c := attachConsole(t, p)
	defer c.Close()
	readN(t, c, len("ready\n"))

	// A Read blocked when the VM ends must return EOF.
	res := make(chan error, 1)
	go func() { _, err := c.Read(make([]byte, 8)); res <- err }()
	time.Sleep(30 * time.Millisecond)
	p.stop()

	select {
	case err := <-res:
		if err != io.EOF {
			t.Fatalf("blocked Read = %v, want EOF", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked Read not woken when the VM ended")
	}
	if _, err := c.Write([]byte("x")); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Write after the VM ended = %v, want ErrClosedPipe", err)
	}
}

func TestConsoleOutputBeforeAttachIsWaiting(t *testing.T) {
	// The banner was printed long before this client arrived.
	p := startEcho(t)
	time.Sleep(50 * time.Millisecond)
	c := attachConsole(t, p)
	defer c.Close()
	if got := readN(t, c, len("ready\n")); got != "ready\n" {
		t.Fatalf("banner = %q", got)
	}
}

// A guest that never reads its input must not hang a client for ever.
func TestConsoleStalledGuestFailsTheWriteInsteadOfHanging(t *testing.T) {
	p := startFake(t, "boot", patient) // prints a banner, never reads stdin
	waitStatus(t, p, vm.StatusRunning)
	p.writeTimeout = 100 * time.Millisecond
	c := attachConsole(t, p)
	defer c.Close()

	big := bytes.Repeat([]byte("x"), 4<<20) // far more than a pipe holds
	start := time.Now()
	n, err := c.Write(big)
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline error", err)
	}
	if n <= 0 || n >= len(big) {
		t.Fatalf("n = %d, want a partial write", n)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("took %v", took)
	}
	if got := p.snapshot().Status; got != vm.StatusRunning {
		t.Fatalf("a stalled write changed the VM to %s", got)
	}
}

func TestConsoleStalledGuestDoesNotLockOutLaterWriters(t *testing.T) {
	p := startFake(t, "boot", patient)
	waitStatus(t, p, vm.StatusRunning)
	p.writeTimeout = 100 * time.Millisecond

	// Fill the pipe, then queue several more writers behind the stall.
	c := attachConsole(t, p)
	c.Write(bytes.Repeat([]byte("x"), 4<<20))

	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Write([]byte("more")); err == nil {
				t.Error("write into a full pipe succeeded")
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	waitClosed(t, done, "queued writers to give up")
}

// Four clients typing at once, as in a shared terminal. Each write must reach
// the guest whole.
func TestConsoleConcurrentWritesDoNotInterleave(t *testing.T) {
	p := startEcho(t)
	c := attachConsole(t, p)
	defer c.Close()
	readN(t, c, len("ready\n"))

	const writers, lines, width = 4, 40, 300
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(ch byte) {
			defer wg.Done()
			line := append(bytes.Repeat([]byte{ch}, width), '\n')
			for i := 0; i < lines; i++ {
				if _, err := c.Write(line); err != nil {
					t.Errorf("Write: %v", err)
					return
				}
			}
		}('A' + byte(w))
	}

	got := readN(t, c, writers*lines*(width+1))
	wg.Wait()
	for _, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if len(line) != width || strings.Trim(line, line[:1]) != "" {
			t.Fatalf("interleaved line: %q", line)
		}
	}
}

// start creates a pipe by hand, so every path must close it.
func TestProcessLeavesNoFileDescriptorsBehind(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("needs /proc")
	}
	count := func() int {
		ents, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(ents)
	}
	time.Sleep(100 * time.Millisecond)
	before := count()

	cycle := func() {
		for _, mode := range []string{"boot", "echo", "poweroff", "crash"} {
			p := startFake(t, mode, patient)
			if c, err := p.attach(); err == nil {
				c.Write([]byte("hi\n"))
				c.Close()
			}
			p.stop()
		}
		bad := newProcess(context.Background(), func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
			return exec.CommandContext(ctx, "/nonexistent/qemu")
		}, "x", okSpec(), nil, time.Now())
		if err := bad.start(patient); err == nil {
			t.Fatal("start of a missing binary succeeded")
		}
	}
	for i := 0; i < 5; i++ {
		cycle()
	}

	deadline := time.Now().Add(3 * time.Second)
	for count() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if after := count(); after > before {
		t.Fatalf("file descriptors: %d before, %d after 25 processes", before, after)
	}
}

// Writes are serialised by a lock. Holding it from the test shows a writer
// waits its turn instead of racing.
func TestConsoleWriteWaitsForTheInputLock(t *testing.T) {
	p := startEcho(t)
	c := attachConsole(t, p)
	defer c.Close()
	readN(t, c, len("ready\n"))

	p.stdinMu.Lock()
	wrote := make(chan struct{})
	go func() { c.Write([]byte("queued\n")); close(wrote) }()

	select {
	case <-wrote:
		p.stdinMu.Unlock()
		t.Fatal("Write went ahead without taking the input lock")
	case <-time.After(100 * time.Millisecond):
	}
	p.stdinMu.Unlock()
	waitClosed(t, wrote, "the queued write after unlocking")
	if got := readN(t, c, len("queued\n")); got != "queued\n" {
		t.Fatalf("got %q", got)
	}
}

// A client blocked writing into a guest that never reads, when the VM is
// killed, gets the same error as any other write to a finished VM.
func TestConsoleWriteBlockedWhenTheVMDiesReportsClosedPipe(t *testing.T) {
	p := startFake(t, "boot", patient) // never reads its input
	waitStatus(t, p, vm.StatusRunning)
	p.writeTimeout = time.Minute // so only the kill can end the write
	c := attachConsole(t, p)
	defer c.Close()

	res := make(chan error, 1)
	go func() { _, err := c.Write(bytes.Repeat([]byte("x"), 4<<20)); res <- err }()
	time.Sleep(100 * time.Millisecond) // let it fill the pipe and block
	p.stop()

	select {
	case err := <-res:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatalf("err = %v, want ErrClosedPipe", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked Write not released when the VM died")
	}
}
