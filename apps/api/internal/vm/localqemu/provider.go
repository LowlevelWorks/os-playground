package localqemu

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

const (
	defaultMaxVMs       = 2
	defaultStartTimeout = 30 * time.Second
)

// ErrClosed is returned by Create after Close.
var ErrClosed = errors.New("localqemu: provider is closed")

// Config configures a Provider. The zero value is usable except that QEMU
// must be installed.
type Config struct {
	// QEMUBin is the QEMU executable: a name found on PATH, or a path. Empty
	// means qemu-system-x86_64.
	QEMUBin string

	// Options are the per-VM QEMU settings: CPUs, accelerator, memory limit.
	Options

	// MaxVMs is how many VMs may exist at once, running or finished but not
	// yet deleted. Zero means 2. This is a development provider on one
	// machine: the limit is deliberately small, and there is no "unlimited".
	MaxVMs int

	// StartTimeout is how long a VM may take to print its first byte on the
	// serial console before it is failed and killed. Zero means 30 seconds.
	StartTimeout time.Duration

	// Logger receives VM lifecycle events, including QEMU's own error text
	// when a VM fails. Nil discards them.
	Logger *slog.Logger
}

// Provider is a vm.Provider that runs each VM as a local QEMU process.
//
// It is for development and tests. It gives each VM its own process group
// and kills the whole group on Delete or Close, but it cannot protect VMs
// from the server being killed outright (SIGKILL, a crash): QEMU processes
// would then keep running. Their command line carries "-name <vm id>", so
// find strays with pgrep -f. A Provider must be closed with Close.
//
// A VM that has stopped or failed keeps its slot until Delete, like any other
// VM, so the application is expected to delete finished sessions.
type Provider struct {
	qemuBin      string
	opts         Options
	maxVMs       int
	startTimeout time.Duration
	log          *slog.Logger
	command      commandFunc
	now          func() time.Time

	// ctx is the parent of every VM's context. Close cancels it.
	ctx    context.Context
	cancel context.CancelFunc

	mu     sync.Mutex
	vms    map[string]*entry
	closed bool
}

// entry is a VM as the provider tracks it.
type entry struct {
	proc *process

	// deleting is set when Delete begins. From then on the VM is gone as far
	// as Get and Console are concerned, but it keeps its slot (and its ID)
	// until the process is really dead, so a new VM never starts while the old
	// one is still dying.
	deleting bool
}

var _ vm.Provider = (*Provider)(nil)

// New checks the configuration and that QEMU exists, so a mistake stops the
// server at startup instead of failing the first user's session.
func New(cfg Config) (*Provider, error) {
	opts, err := cfg.Options.normalized()

	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	if cfg.MaxVMs < 0 {
		errs = append(errs, fmt.Errorf("MaxVMs=%d: must not be negative", cfg.MaxVMs))
	}
	if cfg.StartTimeout < 0 {
		errs = append(errs, fmt.Errorf("StartTimeout=%s: must not be negative", cfg.StartTimeout))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	if cfg.MaxVMs == 0 {
		cfg.MaxVMs = defaultMaxVMs
	}
	if cfg.StartTimeout == 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	bin := cfg.QEMUBin
	if bin == "" {
		bin = "qemu-system-x86_64"
	}
	path, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("localqemu: QEMU not found: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &Provider{
		qemuBin:      path,
		opts:         opts,
		maxVMs:       cfg.MaxVMs,
		startTimeout: cfg.StartTimeout,
		log:          cfg.Logger,
		command:      exec.CommandContext,
		now:          time.Now,
		ctx:          ctx,
		cancel:       cancel,
		vms:          make(map[string]*entry),
	}, nil
}

// Create implements vm.Provider. It returns as soon as QEMU has been
// launched; the VM is "running" once it prints its first serial byte.
//
// The VM's life is tied to the Provider, never to ctx: a finished request
// does not stop it.
//
// A problem with the image (not absolute, not clean, or no such file) is
// reported as vm.ErrInvalidSpec, like every other problem with the Spec. The
// image is never typed by a user, so an HTTP handler should treat it as a
// server fault, not as bad input.
func (p *Provider) Create(ctx context.Context, spec vm.Spec) (vm.Instance, error) {
	if err := ctx.Err(); err != nil {
		return vm.Instance{}, err
	}

	args, err := buildArgs(spec, p.opts) // also validates the spec
	if err != nil {
		return vm.Instance{}, err
	}
	if err := checkImageFile(spec.Image); err != nil {
		return vm.Instance{}, err
	}

	// Reserve the ID and a slot before launching anything, so of several
	// simultaneous creates of one ID exactly one starts QEMU.
	proc := newProcess(p.ctx, p.command, p.qemuBin, spec, args, p.now())
	if err := p.register(proc); err != nil {
		proc.cancel() // release the context that nothing will ever use
		return vm.Instance{}, err
	}

	// The caller may have given up while the process was being built. The
	// process must still be ended properly: a Delete may already be waiting.
	if err := ctx.Err(); err != nil {
		proc.abort("create was cancelled before QEMU was launched")
		p.unregister(proc)
		return vm.Instance{}, err
	}
	if err := proc.start(p.startTimeout); err != nil {
		p.unregister(proc)
		return vm.Instance{}, err
	}
	go p.watch(proc)

	p.log.Info("vm started", "id", spec.ID, "image", spec.Image, "memory_mb", spec.MemoryMB)
	return proc.snapshot(), nil
}

// Get implements vm.Provider.
func (p *Provider) Get(ctx context.Context, id string) (vm.Instance, error) {
	if err := ctx.Err(); err != nil {
		return vm.Instance{}, err
	}
	proc, err := p.lookup(id)
	if err != nil {
		return vm.Instance{}, err
	}
	return proc.snapshot(), nil
}

// Delete implements vm.Provider. From the moment it begins, the VM is gone to
// Get and Console. It kills the VM, waits until QEMU is dead, and only then
// frees the slot. Every concurrent Delete of the same VM returns only after
// the process is dead.
//
// If ctx ends first, Delete returns ctx.Err(). The kill was already ordered
// and the slot is released once the process is dead, whether or not anyone
// calls Delete again.
func (p *Provider) Delete(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	e, ok := p.vms[id]
	if ok {
		e.deleting = true
	}
	p.mu.Unlock()
	if !ok {
		return nil // idempotent: nothing to delete
	}

	e.proc.cancel()
	select {
	case <-e.proc.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if p.unregister(e.proc) { // only the caller that frees the slot reports it
		p.log.Info("vm deleted", "id", id)
	}
	return nil
}

// Console implements vm.Provider.
func (p *Provider) Console(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	proc, err := p.lookup(id)
	if err != nil {
		return nil, err
	}
	return proc.attach()
}

// Close kills every VM and waits until each QEMU process is gone, or until
// ctx ends. It is how a graceful shutdown makes sure nothing is left running,
// so give it a deadline of a few seconds. Create fails after Close. It is safe
// to call more than once.
func (p *Provider) Close(ctx context.Context) error {
	p.mu.Lock()
	p.closed = true
	procs := make([]*process, 0, len(p.vms))
	for _, e := range p.vms {
		procs = append(procs, e.proc)
	}
	p.mu.Unlock()

	p.cancel() // every VM's context hangs off this one

	for _, proc := range procs {
		select {
		case <-proc.done:
		case <-ctx.Done():
			return fmt.Errorf("localqemu: close: %w (some VMs may still be running)", ctx.Err())
		}
	}
	return nil
}

// lookup finds a VM that callers may see: not one being deleted.
func (p *Provider) lookup(id string) (*process, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.vms[id]
	if !ok || e.deleting {
		return nil, fmt.Errorf("%w: %s", vm.ErrNotFound, id)
	}
	return e.proc, nil
}

// register reserves proc's ID and a slot.
func (p *Provider) register(proc *process) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case p.closed:
		return ErrClosed
	case p.vms[proc.id] != nil:
		return fmt.Errorf("%w: %s", vm.ErrAlreadyExists, proc.id)
	case len(p.vms) >= p.maxVMs:
		return fmt.Errorf("%w: limit is %d", vm.ErrCapacity, p.maxVMs)
	}
	p.vms[proc.id] = &entry{proc: proc}
	return nil
}

// unregister frees proc's slot, unless the ID now belongs to a newer VM. It
// reports whether it freed the slot.
func (p *Provider) unregister(proc *process) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e := p.vms[proc.id]; e != nil && e.proc == proc {
		delete(p.vms, proc.id)
		return true
	}
	return false
}

// watch logs how a VM ended, and finishes a deletion nobody waited for: a
// Delete whose caller gave up leaves the VM hidden and its slot held, and this
// releases the slot as soon as the process is dead. A VM that was not being
// deleted stays registered, so its final state can still be read.
//
// A failure's reason holds QEMU's own words, which can include host paths, so
// it goes to the log and not to users.
func (p *Provider) watch(proc *process) {
	<-proc.done
	got := proc.snapshot()

	level := slog.LevelInfo
	if got.Status == vm.StatusFailed {
		level = slog.LevelWarn
	}
	p.log.Log(context.Background(), level, "vm ended",
		"id", got.ID, "status", got.Status, "reason", got.Reason)

	p.mu.Lock()
	if e := p.vms[proc.id]; e != nil && e.proc == proc && e.deleting {
		delete(p.vms, proc.id)
	}
	p.mu.Unlock()
}

func checkImageFile(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%w: image: %v", vm.ErrInvalidSpec, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%w: image %q is not a regular file", vm.ErrInvalidSpec, path)
	}
	return nil
}
