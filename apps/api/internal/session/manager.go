// Package session is the application's record of the VMs it created. It sits
// between the HTTP handlers and a vm.Provider: it chooses every VM's spec,
// hides a VM from clients once it is being deleted, and removes VMs that have
// finished, sat unused, or lived too long. Every VM belongs to the Owner that
// created it, and nobody else can see it.
//
// The provider has no List and frees a VM's capacity slot only on Delete, so
// without this bookkeeping a crashed VM would hold its slot forever.
package session

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
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

// ErrQuota matches every *QuotaError.
var ErrQuota = errors.New("session: per-client VM limit reached")

// QuotaError is Create refusing a client that already holds its share.
type QuotaError struct {
	// Yours is true when one of those VMs, or a create still in flight,
	// belongs to the caller's own token.
	Yours bool
	// ID is the caller's oldest live VM. It is empty unless Yours, and empty
	// while the caller's only VM is still being created.
	ID string
}

func (e *QuotaError) Error() string        { return ErrQuota.Error() }
func (e *QuotaError) Is(target error) bool { return target == ErrQuota }

// Owner is who is asking. Hash is the hex SHA-256 of the browser's token, so
// the manager never holds a token. Client is the network address the quota
// counts against: several owners can share one.
type Owner struct {
	Hash   string
	Client string
}

type Config struct {
	Provider     vm.Provider
	Image        string // absolute path, already validated by config
	MemoryMB     int
	MaxPerClient int           // live VMs one Client may hold, counting creates in flight
	IdleTimeout  time.Duration // no browser attached for this long -> delete
	MaxDuration  time.Duration // hard lifetime -> delete
	Grace        time.Duration // a finished VM stays readable this long; zero means 30s
	Logger       *slog.Logger  // nil discards
}

// Manager is safe for concurrent use. It never calls the provider while
// holding its lock: provider calls can block for seconds.
type Manager struct {
	provider         vm.Provider
	image            string
	memoryMB         int
	perClient        int
	idle, max, grace time.Duration
	log              *slog.Logger

	now   func() time.Time // replaced in tests
	newID func() string    // replaced in tests

	mu       sync.Mutex
	sessions map[string]*session
	pending  map[string]map[string]int // client -> owner hash -> creates in flight
}

type session struct {
	owner         string // Owner.Hash
	client        string // Owner.Client
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
	if cfg.MaxPerClient <= 0 {
		errs = append(errs, fmt.Errorf("session: MaxPerClient=%d: must be greater than 0", cfg.MaxPerClient))
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
		provider:  cfg.Provider,
		image:     cfg.Image,
		memoryMB:  cfg.MemoryMB,
		perClient: cfg.MaxPerClient,
		idle:      cfg.IdleTimeout,
		max:       cfg.MaxDuration,
		grace:     cfg.Grace,
		log:       cfg.Logger,
		now:       time.Now,
		newID:     newVMID,
		sessions:  make(map[string]*session),
		pending:   make(map[string]map[string]int),
	}, nil
}

// newVMID need not be secret, since the owner check guards every VM, but an
// unguessable ID is a second line of defence.
func newVMID() string {
	b := make([]byte, 10)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand failing means the OS is broken
	}
	return "vm-" + hex.EncodeToString(b)
}

// Create starts a VM for o. The caller never chooses the ID, image or size.
// A client at its limit gets a *QuotaError before the provider is asked;
// provider errors come back unchanged, so errors.Is(err, vm.ErrCapacity) works.
//
// The slot is reserved before the provider is called, because that call can
// take seconds: without the reservation, concurrent creates from one client
// would all pass the check. The session is stored in the same critical
// section that drops the reservation, so the VM is never counted twice or not
// at all.
func (m *Manager) Create(ctx context.Context, o Owner) (vm.Instance, error) {
	m.mu.Lock()
	if err := m.checkQuota(o); err != nil {
		m.mu.Unlock()
		return vm.Instance{}, err
	}
	m.reserve(o)
	m.mu.Unlock()

	spec := vm.Spec{ID: m.newID(), Image: m.image, MemoryMB: m.memoryMB}
	inst, err := m.provider.Create(ctx, spec)

	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	if err == nil {
		m.sessions[inst.ID] = &session{owner: o.Hash, client: o.Client, created: now, idleSince: now}
	}
	m.unreserve(o)
	if err != nil {
		return vm.Instance{}, err
	}
	return inst, nil
}

// checkQuota counts o.Client's live VMs and creates in flight. VMs being
// deleted do not count: they are on their way out. The caller holds m.mu.
func (m *Manager) checkQuota(o Owner) error {
	held := 0
	var oldestID string
	var oldest time.Time
	for id, s := range m.sessions {
		if s.deleting || s.client != o.Client {
			continue
		}
		held++
		if !owns(s, o) {
			continue
		}
		if oldestID == "" || s.created.Before(oldest) || (s.created.Equal(oldest) && id < oldestID) {
			oldestID, oldest = id, s.created
		}
	}
	for _, n := range m.pending[o.Client] {
		held += n
	}
	if held < m.perClient {
		return nil
	}

	switch {
	case oldestID != "":
		return &QuotaError{Yours: true, ID: oldestID}
	case m.pending[o.Client][o.Hash] > 0:
		return &QuotaError{Yours: true}
	default:
		return &QuotaError{}
	}
}

// reserve and unreserve count o's creates in flight. Empty maps are removed so
// the map does not grow with every client ever seen. The caller holds m.mu.
func (m *Manager) reserve(o Owner) {
	byOwner := m.pending[o.Client]
	if byOwner == nil {
		byOwner = make(map[string]int)
		m.pending[o.Client] = byOwner
	}
	byOwner[o.Hash]++
}

func (m *Manager) unreserve(o Owner) {
	byOwner := m.pending[o.Client]
	if byOwner[o.Hash]--; byOwner[o.Hash] <= 0 {
		delete(byOwner, o.Hash)
	}
	if len(byOwner) == 0 {
		delete(m.pending, o.Client)
	}
}

// owns compares in constant time. An empty hash owns nothing.
func owns(s *session, o Owner) bool {
	return o.Hash != "" && subtle.ConstantTimeCompare([]byte(s.owner), []byte(o.Hash)) == 1
}

// Get reports a VM that o created and the manager is not deleting. Any other
// VM, whoever owns it, is ErrNotFound.
func (m *Manager) Get(ctx context.Context, id string, o Owner) (vm.Instance, error) {
	if !m.visible(id, o) {
		return vm.Instance{}, notFound(id)
	}
	return m.provider.Get(ctx, id)
}

// Console attaches to a VM's console. The session counts as in use from before
// the provider is asked, so Reap cannot delete it as idle in the gap between
// the provider attaching and this method recording it.
func (m *Manager) Console(ctx context.Context, id string, o Owner) (io.ReadWriteCloser, error) {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || s.deleting || !owns(s, o) {
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
// retry after a failed attempt must do real work. A VM that is gone, or not
// o's, is ErrNotFound.
func (m *Manager) Delete(ctx context.Context, id string, o Owner) error {
	m.mu.Lock()
	s, ok := m.sessions[id]
	if !ok || !owns(s, o) {
		m.mu.Unlock()
		return notFound(id)
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

func (m *Manager) visible(id string, o Owner) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return ok && !s.deleting && owns(s, o)
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
