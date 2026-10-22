package localqemu

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// console is what a client holds while it is attached to a VM: reading gives
// the guest's serial output, writing gives it keyboard input.
//
// It follows the vm.Provider contract. Close detaches and never stops the VM.
// When the VM ends, reads return io.EOF and writes fail.
type console struct {
	p      *process
	r      *sinkReader
	closed atomic.Bool
}

// attach connects a console to a running VM. Only one can be attached at a
// time. Output produced since the last reader left, or since boot for the
// first one, is waiting for it.
func (p *process) attach() (io.ReadWriteCloser, error) {
	if st := p.snapshot().Status; st != vm.StatusRunning {
		return nil, fmt.Errorf("%w: %s is %s", vm.ErrNotRunning, p.id, st)
	}
	r, err := p.out.attach()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, p.id)
	}
	return &console{p: p, r: r}, nil
}

// Read blocks until the guest prints something, the VM ends (io.EOF), or the
// console is closed (io.ErrClosedPipe).
func (c *console) Read(b []byte) (int, error) { return c.r.Read(b) }

// Write sends input to the guest. A write is never split with another
// client's: it is one call to the pipe, under a lock.
//
// A guest that has stopped reading its serial port cannot block a client
// forever: a write that stalls for too long fails with an error wrapping
// os.ErrDeadlineExceeded.
func (c *console) Write(b []byte) (int, error) {
	if c.closed.Load() {
		return 0, io.ErrClosedPipe // a stale client must not type into a new session
	}
	if len(b) == 0 {
		return 0, nil
	}
	return c.p.writeStdin(b)
}

// Close detaches this console. The VM keeps running, and another console can
// attach. It is safe to call more than once.
func (c *console) Close() error {
	c.closed.Store(true)
	return c.r.Close()
}

// ended reports whether the process has finished.
func (p *process) ended() bool {
	select {
	case <-p.done:
		return true
	default:
		return false
	}
}

func (p *process) writeStdin(b []byte) (int, error) {
	p.stdinMu.Lock()
	defer p.stdinMu.Unlock()

	if p.ended() {
		return 0, io.ErrClosedPipe
	}
	p.stdin.SetWriteDeadline(time.Now().Add(p.writeTimeout))
	n, err := p.stdin.Write(b)
	switch {
	case err == nil:
		return n, nil
	case p.ended(), errors.Is(err, syscall.EPIPE), errors.Is(err, os.ErrClosed):
		// The VM died under the write. The pipe breaks a moment before the
		// waiter records the end, so the pipe's own errors count too.
		return n, io.ErrClosedPipe
	case errors.Is(err, os.ErrDeadlineExceeded):
		return n, fmt.Errorf("console input stalled, the guest is not reading: %w", err)
	}
	return n, err
}
