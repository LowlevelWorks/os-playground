// Package session is the application's record of the VMs it created. It sits
// between the HTTP handlers and the Store: it chooses every session's spec,
// hides a session from clients once it is being deleted, and lets only the
// Owner that created a session see it.
//
// Handlers change desired state only (ADR 0010, section 2): Create inserts a
// row and Delete marks it, and neither calls the provider. A reconciler, not
// yet written (stage 3.4), boots and removes the VMs. Console is the one
// exception: it reaches the provider directly once ownership is checked.
//
// Store is the durable record: PostgreSQL in production, MemStore in tests.
// Both must pass storetest.Run.
package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// defaultProfile is the only profile until profiles are configurable.
const defaultProfile = "default"

// Console input is recorded at most this often per console, so idle
// accuracy is about this long and the database sees few writes (ADR 0010,
// section 7). touchTimeout bounds one write.
const (
	activityThrottle = 15 * time.Second
	touchTimeout     = 5 * time.Second
)

// ErrQuota matches every *QuotaError.
var ErrQuota = errors.New("session: quota reached")

// QuotaError refuses a new session because the caller's owner or client
// already holds its share. ClientWide tells which limit was reached; Yours
// tells whether the caller can resume a session of its own instead.
type QuotaError struct {
	// Yours is true when the caller's own token holds a live session.
	Yours bool
	// ID is the caller's oldest live session. It is empty unless Yours.
	ID string

	// ClientWide is set when the per-client limit was reached rather than
	// the per-user one. It is independent of Yours.
	ClientWide bool
}

func (e *QuotaError) Error() string { return ErrQuota.Error() }

// Is makes every *QuotaError match ErrQuota, so callers that only need to
// know "over quota" can use errors.Is.
func (e *QuotaError) Is(target error) bool { return target == ErrQuota }

// Owner is who is asking. Hash is the hex SHA-256 of the browser's token, so
// the manager never holds a token. Client is the network address the quota
// counts against: several owners can share one.
type Owner struct {
	Hash   string
	Client string
}

// Config configures a Manager. Every field except Wake and Logger is required; New
// reports all missing or invalid fields at once.
type Config struct {
	Store Store
	// Provider is used for Console only. Creating and deleting VMs is the
	// reconciler's job.
	Provider     vm.Provider
	Image        string        // absolute path, already validated by config
	MemoryMB     int           // guest RAM, stored in each session's spec
	Capacity     int           // sessions that may hold a VM at once
	MaxPerOwner  int           // live sessions one Owner.Hash may hold, on any Client
	MaxPerClient int           // live sessions one Client may hold, across owners
	IdleTimeout  time.Duration // no browser attached for this long -> delete
	MaxDuration  time.Duration // hard lifetime -> delete
	// Wake, if set, is called after a session is admitted or marked for
	// deletion, so the reconciler acts at once instead of at its next poll.
	Wake   func()
	Logger *slog.Logger // nil discards
}

// Manager is safe for concurrent use: it holds no state of its own beyond
// its configuration, and the Store serialises admissions.
type Manager struct {
	store    Store
	provider vm.Provider
	image    string
	spec     json.RawMessage // the same for every session; marshalled once in New
	limits   Limits
	idle     time.Duration
	max      time.Duration
	wake     func()
	log      *slog.Logger

	now func() time.Time // replaced in tests
}

// vmSpec is what Session.Spec holds: what the reconciler needs to build a
// vm.Spec, apart from the ID (the session ID) and the image (ImageRef).
type vmSpec struct {
	MemoryMB int `json:"memory_mb"`
}

// New checks the configuration. A zero timeout would expire every session at
// once, so it is rejected, not defaulted.
func New(cfg Config) (*Manager, error) {
	var errs []error
	if cfg.Store == nil {
		errs = append(errs, errors.New("session: Store is required"))
	}
	if cfg.Provider == nil {
		errs = append(errs, errors.New("session: Provider is required"))
	}
	if cfg.Image == "" {
		errs = append(errs, errors.New("session: Image is required"))
	}
	if cfg.MemoryMB <= 0 {
		errs = append(errs, fmt.Errorf("session: MemoryMB=%d: must be greater than 0", cfg.MemoryMB))
	}
	if cfg.Capacity <= 0 {
		errs = append(errs, fmt.Errorf("session: Capacity=%d: must be greater than 0", cfg.Capacity))
	}
	if cfg.MaxPerOwner <= 0 {
		errs = append(errs, fmt.Errorf("session: MaxPerOwner=%d: must be greater than 0", cfg.MaxPerOwner))
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
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	spec, err := json.Marshal(vmSpec{MemoryMB: cfg.MemoryMB})
	if err != nil {
		return nil, fmt.Errorf("session: marshal spec: %w", err)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if cfg.Wake == nil {
		cfg.Wake = func() {}
	}
	return &Manager{
		store:    cfg.Store,
		provider: cfg.Provider,
		image:    cfg.Image,
		spec:     spec,
		limits: Limits{
			Capacity:  cfg.Capacity,
			PerOwner:  cfg.MaxPerOwner,
			PerClient: cfg.MaxPerClient,
		},
		idle: cfg.IdleTimeout,
		max:  cfg.MaxDuration,
		wake: cfg.Wake,
		log:  cfg.Logger,
		now:  time.Now,
	}, nil
}

// Create admits a session for o and returns it as provisioning. The caller
// never chooses the ID, image or size. Nothing is booted here: the row is
// the reservation, and the reconciler creates the VM (ADR 0010, section 5).
//
// The Store's errors come back unchanged, so errors.As(*QuotaError) and
// errors.Is(ErrCapacity) or errors.Is(ErrBusy) work for the caller.
func (m *Manager) Create(ctx context.Context, o Owner) (vm.Instance, error) {
	s, err := m.store.Admit(ctx, NewSession{
		OwnerID:     o.Hash,
		ClientKey:   o.Client,
		Profile:     defaultProfile,
		ImageRef:    m.image,
		Spec:        m.spec,
		MaxDuration: m.max,
		IdleTimeout: m.idle,
	}, m.limits)
	if err != nil {
		return vm.Instance{}, err
	}
	m.wake()
	return toInstance(s), nil
}

// Get reports a session that o owns and has not asked to delete. Any other
// session, whoever owns it, is ErrNotFound.
func (m *Manager) Get(ctx context.Context, id string, o Owner) (vm.Instance, error) {
	s, err := m.visible(ctx, id, o)
	if err != nil {
		return vm.Instance{}, err
	}
	return toInstance(s), nil
}

// Delete marks the session for deletion and returns at once; the reconciler
// removes the VM. The session is hidden from that moment, so deleting it
// again is ErrNotFound, as is a session that is missing or not o's.
func (m *Manager) Delete(ctx context.Context, id string, o Owner) error {
	s, err := m.visible(ctx, id, o)
	if err != nil {
		return err
	}
	// RequestDelete is idempotent, so two racing Deletes both succeed.
	if err := m.store.RequestDelete(ctx, s.ID, o.Hash); err != nil {
		if errors.Is(err, ErrNotFound) {
			return notFound(id)
		}
		return fmt.Errorf("session: request delete: %w", err)
	}
	m.wake()
	return nil
}

// Console attaches to the VM of a session o owns. The provider refuses a VM
// that does not exist yet or is not running, and a second attach while it
// can hold only one. Input written to the console counts as activity and
// pushes the idle deadline out; attaching alone does not.
//
// TODO: the attach lease in the row (ADR 0010, section 7). It matters only
// with several replicas, which the local provider does not support.
func (m *Manager) Console(ctx context.Context, id string, o Owner) (io.ReadWriteCloser, error) {
	s, err := m.visible(ctx, id, o)
	if err != nil {
		return nil, err
	}
	c, err := m.provider.Console(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	return &activeConsole{ReadWriteCloser: c, m: m, id: s.ID}, nil
}

// activeConsole records input as activity, throttled, without ever making a
// keystroke wait for the database: the write happens in the background, one
// at a time.
type activeConsole struct {
	io.ReadWriteCloser
	m  *Manager
	id string

	mu       sync.Mutex
	last     time.Time
	inFlight bool
}

func (c *activeConsole) Write(p []byte) (int, error) {
	c.touch()
	return c.ReadWriteCloser.Write(p)
}

func (c *activeConsole) touch() {
	c.mu.Lock()
	now := c.m.now()
	if c.inFlight || (!c.last.IsZero() && now.Sub(c.last) < activityThrottle) {
		c.mu.Unlock()
		return
	}
	c.inFlight, c.last = true, now
	c.mu.Unlock()

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), touchTimeout)
		defer cancel()
		if err := c.m.store.TouchActivity(ctx, c.id, c.m.idle); err != nil {
			c.m.log.Warn("record console activity failed", "session_id", c.id, "err", err)
		}
		c.mu.Lock()
		c.inFlight = false
		c.mu.Unlock()
	}()
}

// visible returns the session if o owns it and it is not being deleted. The
// Store returns sessions marked for deletion too; hiding them is this
// layer's choice (ADR 0010, section 4).
func (m *Manager) visible(ctx context.Context, id string, o Owner) (Session, error) {
	s, err := m.store.Get(ctx, id, o.Hash)
	switch {
	case errors.Is(err, ErrNotFound):
		return Session{}, notFound(id)
	case err != nil:
		return Session{}, fmt.Errorf("session: get: %w", err)
	case s.DesiredDeleted || s.Status == StatusDeleting || s.Status == StatusDeleted:
		return Session{}, notFound(id)
	}
	return s, nil
}

// toInstance is the session as the API shows it. Reason stays inside the
// server: the HTTP layer does not send it.
func toInstance(s Session) vm.Instance {
	return vm.Instance{
		ID:        s.ID,
		Status:    s.Status.public(),
		Image:     s.ImageRef,
		Reason:    s.FailureReason,
		CreatedAt: s.CreatedAt,
	}
}

// public maps a stored status to the one the API shows (ADR 0010, section
// 4). DELETING and DELETED never get here: visible hides them. STOPPING is
// not used until the product has a stopped state.
func (s Status) public() vm.Status {
	switch s {
	case StatusCreated, StatusProvisioning:
		return vm.StatusProvisioning
	case StatusStarting:
		return vm.StatusStarting
	case StatusRunning:
		return vm.StatusRunning
	case StatusStopped:
		return vm.StatusStopped
	case StatusFailed:
		return vm.StatusFailed
	default:
		return vm.StatusUnknown
	}
}

func notFound(id string) error { return fmt.Errorf("%w: %s", vm.ErrNotFound, id) }
