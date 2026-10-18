package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// ErrNotAttached is returned by Guest when no console is attached.
var ErrNotAttached = errors.New("fake: no console attached")

var errVMEnded = errors.New("fake: vm has ended")

// Option configures a Fake.
type Option func(*Fake)

// WithCapacity limits how many VMs can exist at once. Zero means unlimited.
func WithCapacity(n int) Option { return func(f *Fake) { f.capacity = n } }

// WithAutoRun makes VMs finish booting instantly: Create still reports
// provisioning, but the VM is already running when next observed. Without it a
// test drives the lifecycle by hand with Advance.
func WithAutoRun() Option { return func(f *Fake) { f.autoRun = true } }

// WithClock replaces time.Now, which is used for Instance.CreatedAt.
func WithClock(now func() time.Time) Option { return func(f *Fake) { f.now = now } }

// Fake is an in-memory vm.Prvider. It is safe for concurrent use.
type Fake struct {
	mu       sync.Mutex
	vms      map[string]*entry
	capacity int
	autoRun  bool
	now      func() time.Time
}

var _ vm.Provider = (*Fake)(nil)

type entry struct {
	inst    vm.Instance
	console *console // nil when nothing is attached
}

// end terminates any attached console because the VM is gone.
func (e *entry) end() {
	if e.console != nil {
		e.console.endByVM()
		e.console = nil
	}
}

// New returns an empty Fake.
func New(opts ...Option) *Fake {
	f := &Fake{vms: make(map[string]*entry), now: time.Now}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

// Create implements vm.Provider.
func (f *Fake) Create(ctx context.Context, spec vm.Spec) (vm.Instance, error) {
	if err := ctx.Err(); err != nil {
		return vm.Instance{}, err
	}
	if err := spec.Validate(); err != nil {
		return vm.Instance{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if _, ok := f.vms[spec.ID]; ok {
		return vm.Instance{}, fmt.Errorf("%w: %s", vm.ErrAlreadyExists, spec.ID)
	}
	if f.capacity > 0 && len(f.vms) >= f.capacity {
		return vm.Instance{}, fmt.Errorf("%w: limit is %d", vm.ErrCapacity, f.capacity)
	}

	inst := vm.Instance{
		ID:        spec.ID,
		Status:    vm.StatusProvisioning,
		Image:     spec.Image,
		CreatedAt: f.now(),
	}
	stored := inst
	if f.autoRun {
		stored.Status = vm.StatusRunning
	}
	f.vms[spec.ID] = &entry{inst: stored}
	return inst, nil
}

// Get implements vm.Provider.
func (f *Fake) Get(ctx context.Context, id string) (vm.Instance, error) {
	if err := ctx.Err(); err != nil {
		return vm.Instance{}, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return vm.Instance{}, notFound(id)
	}
	return e.inst, nil // Instance is a value type: the caller gets a copy
}

// Delete implements vm.Provider.
func (f *Fake) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return nil // idempotent
	}
	e.end()
	delete(f.vms, id)
	return nil
}

// Console implements vm.Provider. Only one console can be attached at a time.
func (f *Fake) Console(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return nil, notFound(id)
	}
	if e.inst.Status != vm.StatusRunning {
		return nil, fmt.Errorf("%w: %s is %s", vm.ErrNotRunning, id, e.inst.Status)
	}
	if e.console != nil {
		return nil, fmt.Errorf("%w: %s", vm.ErrAlreadyAttached, id)
	}

	c := newConsole()
	e.console = c
	return &clientConn{f: f, id: id, c: c}, nil
}

// Advance moves a VM one step forward: provisioning, starting, running. It
// fails if the VM is already running or finished.
func (f *Fake) Advance(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return notFound(id)
	}
	switch e.inst.Status {
	case vm.StatusProvisioning:
		e.inst.Status = vm.StatusStarting
	case vm.StatusStarting:
		e.inst.Status = vm.StatusRunning
	default:
		return fmt.Errorf("fake: cannot advance %s from %s", id, e.inst.Status)
	}
	return nil
}

// Finish ends a VM that has not finished yet, as stopped or failed, with a
// reason. An attached console ends. The VM keeps existing, and holds its
// capacity slot, until Delete.
func (f *Fake) Finish(id string, status vm.Status, reason string) error {
	if !status.IsTerminal() {
		return fmt.Errorf("fake: Finish needs a terminal status, got %q", status)
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return notFound(id)
	}
	if e.inst.Status.IsTerminal() {
		return fmt.Errorf("fake: %s already %s", id, e.inst.Status)
	}
	e.inst.Status = status
	e.inst.Reason = reason
	e.end()
	return nil
}

// Guest returns the guest's side of the attached console: reading yields what
// the client wrote, and writing is what the client reads. It returns
// ErrNotAttached if nothing is attached.
func (f *Fake) Guest(id string) (io.ReadWriteCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	e, ok := f.vms[id]
	if !ok {
		return nil, notFound(id)
	}
	if e.console == nil {
		return nil, ErrNotAttached
	}
	return &guestConn{c: e.console}, nil
}

// Len reports how many VMs exist, including finished ones not yet deleted.
func (f *Fake) Len() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.vms)
}

func notFound(id string) error { return fmt.Errorf("%w: %s", vm.ErrNotFound, id) }

// console is the pair of pipes behind one attachment.
type console struct {
	toGuestR   *io.PipeReader // guest reads what the client wrote
	toGuestW   *io.PipeWriter // client writes
	fromGuestR *io.PipeReader // client reads what the guest wrote
	fromGuestW *io.PipeWriter // guest writes
}

func newConsole() *console {
	c := &console{}
	c.toGuestR, c.toGuestW = io.Pipe()
	c.fromGuestR, c.fromGuestW = io.Pipe()
	return c
}

// detach is the client closing its end. The VM keeps running.
func (c *console) detach() {
	c.toGuestW.Close()
	c.fromGuestR.Close()
}

// endByVM is the VM going away: client reads see io.EOF, writes fail.
func (c *console) endByVM() {
	c.fromGuestW.Close()
	c.toGuestR.CloseWithError(errVMEnded)
}

// clientConn is what Console returns.
type clientConn struct {
	f    *Fake
	id   string
	c    *console
	once sync.Once
}

func (cc *clientConn) Read(p []byte) (int, error)  { return cc.c.fromGuestR.Read(p) }
func (cc *clientConn) Write(p []byte) (int, error) { return cc.c.toGuestW.Write(p) }

// Close detaches only; it never stops the VM.
func (cc *clientConn) Close() error {
	cc.once.Do(func() {
		cc.c.detach()
		cc.f.mu.Lock()
		defer cc.f.mu.Unlock()
		// Clear the flag only if this attachment is still the current one.
		if e, ok := cc.f.vms[cc.id]; ok && e.console == cc.c {
			e.console = nil
		}
	})
	return nil
}

// guestConn is what Guest returns.
type guestConn struct{ c *console }

func (g *guestConn) Read(p []byte) (int, error)  { return g.c.toGuestR.Read(p) }
func (g *guestConn) Write(p []byte) (int, error) { return g.c.fromGuestW.Write(p) }
func (g *guestConn) Close() error                { return g.c.fromGuestW.Close() }
