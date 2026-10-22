// Package session is the application's record of the VMs it created. It sits
// between the HTTP handlers and a vm.Provider: it chooses every VM's spec,
// hides a VM from clients once it is being deleted, and removes VMs that have
// finished, sat unused, or lived too long.
//
// The provider has no List and frees a VM's capacity slot only on Delete, so
// without this bookkeeping a crashed VM would hold its slot forever.
package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

const defaultGrace = 30 * time.Second

type Config struct {
	Provider    vm.Provider
	Image       string // absolute path, already validated by config
	MemoryMB    int
	IdleTimeout time.Duration // no browser attached for this long -> delete
	MaxDuration time.Duration // hard lifetime -> delete
	Grace       time.Duration // a finished VM stays readable this long; zero means 30s
	Logger      *slog.Logger  // nil discards
}

// Manager is safe for concurrent use. It never calls the provider while
// holding its lock: provider calls can block for seconds.
type Manager struct {
	provider         vm.Provider
	image            string
	memoryMB         int
	idle, max, grace time.Duration
	log              *slog.Logger

	now   func() time.Time // replaced in tests
	newID func() string    // replaced in tests

	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	created       time.Time
	attachers     int       // open consoles, plus attach attempts in flight
	idleSince     time.Time // when attachers last became 0 (starts at created)
	terminalSince time.Time // zero until the VM is first seen finished
	deleting      bool      // hidden from clients; Reap finishes the job
}

// New checks the configuration. A zero timeout would make the reaper delete
// every VM on its first pass, so it is rejected, not defaulted.
func New(cfg Config) (*Manager, error) {
	var errs []error
	if cfg.Provider == nil {
		errs = append(errs, errors.New("session: Provider is required"))
	}
	if cfg.Image == "" {
		errs = append(errs, errors.New("session: Image is required"))
	}
	if cfg.MemoryMB <= 0 {
		errs = append(errs, fmt.Errorf("session: MemoryMB=%d: must be greater than 0", cfg.MemoryMB))
	}
	if cfg.IdleTimeout <= 0 {
		errs = append(errs, fmt.Errorf("session: IdleTimeout=%s: must be greater than 0", cfg.IdleTimeout))
	}
	if cfg.MaxDuration <= 0 {
		errs = append(errs, fmt.Errorf("session: MaxDuration=%s: must be greater than 0", cfg.MaxDuration))
	}
	if cfg.Grace < 0 {
		errs = append(errs, fmt.Errorf("session: Grace=%s: must not be negative", cfg.Grace))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	if cfg.Grace == 0 {
		cfg.Grace = defaultGrace
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Manager{
		provider: cfg.Provider,
		image:    cfg.Image,
		memoryMB: cfg.MemoryMB,
		idle:     cfg.IdleTimeout,
		max:      cfg.MaxDuration,
		grace:    cfg.Grace,
		log:      cfg.Logger,
		now:      time.Now,
		newID:    newVMID,
		sessions: make(map[string]*session),
	}, nil
}

// newVMID is the only thing protecting a console until auth exists, so it
// must be unguessable.
func newVMID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the OS is broken
	}
	return "vm-" + hex.EncodeToString(b)
}

// Create starts a VM. The caller never chooses the ID, image or size. Provider
// errors come back unchanged, so errors.Is(err, vm.ErrCapacity) works.
func (m *Manager) Create(ctx context.Context) (vm.Instance, error) {
	spec := vm.Spec{ID: m.newID(), Image: m.image, MemoryMB: m.memoryMB}

	inst, err := m.provider.Create(ctx, spec)
	if err != nil {
		return vm.Instance{}, err
	}

	now := m.now()
	m.mu.Lock()
	m.sessions[inst.ID] = &session{created: now, idleSince: now}
	m.mu.Unlock()
	return inst, nil
}

// Get reports a VM the manager created and is not deleting.
func (m *Manager) Get(ctx context.Context, id string) (vm.Instance, error) {
	if !m.visible(id) {
		return vm.Instance{}, notFound(id)
	}
	return m.provider.Get(ctx, id)
}

// Console attaches to a VM's console. The session counts as in use from before
// the provider is asked, so Reap cannot delete it as idle in the gap between
// the provider attaching and this method recording it.
func (m *Manager) Console(ctx context.Context, id string) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.deleting {
		m.mu.Unlock()
		return nil, notFound(id)
	}
	s.attachers++
	m.mu.Unlock()

	c, err := m.provider.Console(ctx, id)
	if err != nil {
		m.release(id)
		return nil, err
	}
	return &console{ReadWriteCloser: c, release: func() { m.release(id) }}, nil
}

// Delete hides the VM at once and returns when it is really gone. If ctx ends
// first the VM stays hidden and Reap finishes the deletion. Deleting a VM that
// is already being deleted calls the provider again: it is idempotent, and a
// retry after a failed attempt must do real work.
func (m *Manager) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	s.deleting = true
	m.mu.Unlock()

	return m.finishDelete(ctx, id)
}

// finishDelete kills the VM and forgets the session.
func (m *Manager) finishDelete(ctx context.Context, id string) error {
	if err := m.provider.Delete(ctx, id); err != nil && !errors.Is(err, vm.ErrNotFound) {
		return err
	}
	m.forget(id)
	return nil
}

func (m *Manager) forget(id string) {
	m.mu.Lock()
	delete(m.sessions, id)
	m.mu.Unlock()
}

func (m *Manager) visible(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return ok && !s.deleting
}

// release gives back one attach reservation. The idle clock restarts when the
// last one goes.
func (m *Manager) release(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[id]
	if !ok || s.attachers == 0 {
		return
	}
	s.attachers--
	if s.attachers == 0 {
		s.idleSince = m.now()
	}
}

// console releases its reservation exactly once, however often it is closed.
type console struct {
	io.ReadWriteCloser
	release func()
	once    sync.Once
}

func (c *console) Close() error {
	var err error
	c.once.Do(func() {
		err = c.ReadWriteCloser.Close()
		c.release()
	})
	return err
}

// Reap runs one cleanup pass: it finishes deletions that were started but not
// completed, drops sessions the provider no longer knows, and deletes VMs that
// have finished, sat idle, or lived too long.
func (m *Manager) Reap(ctx context.Context) {
	type removal struct{ id, reason string }

	// 1. Snapshot.
	var pending []removal
	var inspect []string
	m.mu.Lock()
	for id, s := range m.sessions {
		if s.deleting {
			pending = append(pending, removal{id, "retry"})
		} else {
			inspect = append(inspect, id)
		}
	}
	m.mu.Unlock()

	// 2. Ask the provider, with the lock released.
	status := make(map[string]vm.Status, len(inspect))
	for _, id := range inspect {
		inst, err := m.provider.Get(ctx, id)
		switch {
		case errors.Is(err, vm.ErrNotFound):
			m.forget(id)
		case err != nil:
			// Try again next pass.
		default:
			status[id] = inst.Status
		}
	}

	// 3. Decide, re-reading each session: it may have changed meanwhile.
	m.mu.Lock()
	now := m.now()
	for id, st := range status {
		s, ok := m.sessions[id]
		if !ok || s.deleting {
			continue
		}
		if reason := m.dueReason(s, st, now); reason != "" {
			s.deleting = true // hidden from now on
			pending = append(pending, removal{id, reason})
		}
	}
	m.mu.Unlock()

	// 4. Delete, with the lock released.
	for _, r := range pending {
		if err := m.finishDelete(ctx, r.id); err != nil {
			m.log.Error("session removal failed; will retry", "vm_id", r.id, "reason", r.reason, "err", err)
			continue
		}
		m.log.Info("session removed", "vm_id", r.id, "reason", r.reason)
	}
}

// dueReason says why a session should be deleted now, or "" if it should not.
// The caller holds m.mu.
func (m *Manager) dueReason(s *session, st vm.Status, now time.Time) string {
	if st.IsTerminal() {
		if s.terminalSince.IsZero() {
			s.terminalSince = now
		}
		if now.Sub(s.terminalSince) >= m.grace {
			return "finished"
		}
	}
	if now.Sub(s.created) >= m.max {
		return "max duration"
	}
	if s.attachers == 0 && now.Sub(s.idleSince) >= m.idle {
		return "idle"
	}
	return ""
}

// Run calls Reap every interval until ctx ends.
func (m *Manager) Run(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.Reap(ctx)
		}
	}
}

func notFound(id string) error { return fmt.Errorf("%w: %s", vm.ErrNotFound, id) }
