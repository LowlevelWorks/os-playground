package localqemu

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

const (
	sinkSize   = 64 << 10        // newest console output kept for a reader
	stderrTail = 4 << 10         // enough for QEMU's complaint, not a log
	waitDelay  = 3 * time.Second // bound on pipes held open by stray children

	// How long one write to the guest's console input may stall before it
	// fails. It bounds how long a guest that has stopped reading can hold up
	// a client, and the next client to attach.
	stdinWriteTimeout = 5 * time.Second
)

// commandFunc builds a command. It is exec.CommandContext in production and
// is replaced in tests.
type commandFunc func(ctx context.Context, name string, args ...string) *exec.Cmd

// process is one QEMU process and everything the provider tracks about it.
//
// Lifecycle:
//
//	starting --first console byte--> running --exit or stop--> stopped | failed
//	starting --no byte in time--------------------------------> failed
//	starting --exit or stop-----------------------------------> stopped | failed
//
// A terminal status is final. Exactly one goroutine (the waiter) ends the
// process's life; exactly one other (the start watcher) can move it from
// starting to running or fail it for staying silent.
//
// Fields above mu are set once in newProcess and never change, so they are
// safe to read without locking. Fields below mu need it.
type process struct {
	id      string
	image   string
	created time.Time

	ctx    context.Context // cancelled by stop, by a start timeout, or by the provider closing
	cancel context.CancelFunc

	cmd    *exec.Cmd
	out    *sink         // QEMU's stdout; its Write never blocks
	stderr *tailBuffer   // last bytes of QEMU's stderr, for failure reasons
	done   chan struct{} // closed by the waiter once the final state is set

	// stdin is QEMU's input. start creates it, before anything else can see
	// the process, and the waiter closes it. Writes go through writeStdin.
	stdin        *os.File
	stdinMu      sync.Mutex // one write at a time, so inputs never interleave
	writeTimeout time.Duration

	mu       sync.Mutex
	status   vm.Status
	reason   string
	exitTime time.Time
}

// newProcess builds and wires a command but does not start it. It holds no
// files or goroutines, so a process that is never started needs no cleanup.
//
// parent bounds the VM's whole life, so it must be the provider's own
// context, never the context of the Create call. Cancelling it, or calling
// p.cancel, kills the VM's process group.
func newProcess(parent context.Context, command commandFunc, bin string, spec vm.Spec, args []string, now time.Time) *process {
	ctx, cancel := context.WithCancel(parent)

	cmd := command(ctx, bin, args...)
	setProcAttr(cmd) // own process group, so one kill reaches every child
	cmd.Cancel = func() error { return killGroup(cmd) }
	cmd.WaitDelay = waitDelay

	p := &process{
		id:      spec.ID,
		image:   spec.Image,
		created: now,
		ctx:     ctx,
		cancel:  cancel,
		cmd:     cmd,
		out:     newSink(sinkSize),
		stderr:  newTailBuffer(stderrTail),
		done:    make(chan struct{}),
		status:  vm.StatusStarting,

		writeTimeout: stdinWriteTimeout,
	}
	cmd.Stdout = p.out
	cmd.Stderr = p.stderr
	return p
}

// start launches QEMU and the two goroutines that supervise it.
//
// If QEMU never prints a byte on the console within startTimeout, the VM is
// failed and killed. On error nothing is left running, and the process is
// already in its final (failed) state, so stop and done are still safe to use.
func (p *process) start(startTimeout time.Duration) error {
	// The input pipe is made here, not with cmd.StdinPipe, because a pipe we
	// own supports write deadlines.
	pr, pw, err := os.Pipe()
	if err != nil {
		return p.failStart(err)
	}
	p.cmd.Stdin = pr
	p.stdin = pw

	err = p.cmd.Start()
	pr.Close() // the child holds its own copy now, or never will
	if err != nil {
		pw.Close()
		return p.failStart(err)
	}

	go p.wait()
	go p.watchStart(startTimeout)
	return nil
}

// abort puts a process that will never run into its final state and releases
// anyone waiting on it. Exactly one of abort, a failed start, or the waiter
// ends a process's life.
func (p *process) abort(reason string) {
	p.cancel()
	p.settle(vm.StatusFailed, reason)
	p.markExited()
	p.out.finish()
	close(p.done)
}

// failStart ends a process whose launch failed.
func (p *process) failStart(err error) error {
	p.abort(fmt.Sprintf("start qemu: %v", err))
	return fmt.Errorf("start qemu: %w", err)
}

// wait is the only goroutine that ends the process's life. The order inside
// it matters: the final status is recorded first, then readers are told the
// output has ended, then done is closed. Anyone who sees EOF or done can
// therefore trust the status they read next.
func (p *process) wait() {
	err := p.cmd.Wait()
	p.stdin.Close() // QEMU is gone; later writes fail at once
	if errors.Is(err, exec.ErrWaitDelay) {
		// QEMU itself exited cleanly; only a stray child held a pipe open.
		err = nil
	}

	switch {
	case p.ctx.Err() != nil, err == nil:
		// We asked it to stop, or the guest powered off by itself.
		p.settle(vm.StatusStopped, "")
	default:
		p.settle(vm.StatusFailed, p.failureReason(err))
	}
	p.markExited()
	p.cancel() // release the context; stop and a timeout already did
	p.out.finish()
	close(p.done)
}

// watchStart turns "first console byte" into the running status, and fails a
// VM that stays silent. It ends as soon as the process is over.
func (p *process) watchStart(timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case <-p.out.firstByte():
		p.markRunning()
	case <-timer.C:
		// A byte may have arrived in the same instant. Prefer it.
		select {
		case <-p.out.firstByte():
			p.markRunning()
			return
		default:
		}
		if p.settle(vm.StatusFailed, fmt.Sprintf("no console output within %s", timeout)) {
			p.cancel() // kill it; the waiter will see the status already settled
		}
	case <-p.done:
	}
}

// markRunning moves starting to running. Any other status stays as it is.
func (p *process) markRunning() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status == vm.StatusStarting {
		p.status = vm.StatusRunning
	}
}

// settle records the end state unless one is already recorded, and reports
// whether it did. This is what keeps a terminal status final: a timeout that
// loses the race with a crash, or a stop that follows a failure, changes
// nothing.
func (p *process) settle(status vm.Status, reason string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.status.IsTerminal() {
		return false
	}
	p.status, p.reason = status, reason
	return true
}

func (p *process) markExited() {
	p.mu.Lock()
	p.exitTime = time.Now()
	p.mu.Unlock()
}

// failureReason describes an unexpected exit, with QEMU's own last words.
// It can contain host paths: log it, and think before showing it to users.
func (p *process) failureReason(err error) string {
	var ee *exec.ExitError
	msg := "qemu: " + err.Error()
	if errors.As(err, &ee) {
		//nolint:staticcheck // QF1008: ExitError has both Error and String; name the one meant
		msg = "qemu: " + ee.ProcessState.String() // "exit status 1", "signal: killed"
	}
	if tail := p.stderr.String(); tail != "" {
		msg += ": " + tail
	}
	return msg
}

// stop kills the VM's process group and waits until it is gone and its final
// status is recorded. It is safe to call more than once and from several
// goroutines, and it returns at once for a process that already ended.
func (p *process) stop() {
	p.cancel()
	<-p.done
}

// snapshot reports the current state.
func (p *process) snapshot() vm.Instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	return vm.Instance{
		ID:        p.id,
		Status:    p.status,
		Image:     p.image,
		Reason:    p.reason,
		CreatedAt: p.created,
	}
}

// lifetime reports how long the process ran. It is only meaningful once done
// is closed.
func (p *process) lifetime() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exitTime.Sub(p.created)
}
