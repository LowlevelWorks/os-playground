package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/os-playground-api/internal/vm"
)

// Status is a session's lifecycle state as stored. It is not vm.Status, which
// is what the API shows; the manager maps one to the other.
type Status string

// Keep this list in the order of the status CHECK in the init migration.
const (
	StatusCreated      Status = "CREATED"
	StatusProvisioning Status = "PROVISIONING"
	StatusStarting     Status = "STARTING"
	StatusRunning      Status = "RUNNING"
	StatusStopping     Status = "STOPPING"
	StatusStopped      Status = "STOPPED"
	StatusFailed       Status = "FAILED"
	StatusDeleting     Status = "DELETING"
	StatusDeleted      Status = "DELETED"
)

var (
	// ErrNotFound and ErrCapacity are the vm errors, so the HTTP layer maps
	// them as before and errors.Is matches either name.
	ErrNotFound = vm.ErrNotFound
	ErrCapacity = vm.ErrCapacity

	// ErrInvalid means the request to the store was malformed. It is a bug in
	// the caller, never the user's fault.
	ErrInvalid = errors.New("session: invalid store request")

	// ErrBusy means admission could not get the lock in time: the server is
	// overloaded, not full. Map it to 503 with Retry-After.
	ErrBusy = errors.New("session: busy, try again")

	// ErrNotAttachable and ErrAttached are the vm errors the terminal
	// handler already maps to 409: the session is not RUNNING (or is being
	// deleted), or another console holds the attach lease.
	ErrNotAttachable = vm.ErrNotRunning
	ErrAttached      = vm.ErrAlreadyAttached

	// ErrLeaseLost means another worker owns the session now: the outcome
	// passed to Complete was discarded, and the caller must drop it too.
	ErrLeaseLost = errors.New("session: work lease lost")
)

// Session is a stored session as the store returns it.
type Session struct {
	ID string

	// OwnerID is the owning user's id (users.id). The store treats it as
	// opaque text.
	OwnerID string

	// ClientKey identifies the network the session was created from.
	ClientKey string

	Profile  string
	ImageRef string

	// Spec is what the reconciler needs to create the VM. Compare it as JSON,
	// not as bytes: PostgreSQL's jsonb does not keep spacing or key order.
	Spec json.RawMessage

	Status Status

	// DesiredDeleted is set by RequestDelete. The VM may still exist; the
	// reconciler removes it.
	DesiredDeleted bool

	FailureReason string

	CreatedAt     time.Time
	ExpiresAt     time.Time
	IdleExpiresAt time.Time
}

// NewSession is what Admit needs to insert a session. The store assigns the ID.
type NewSession struct {
	OwnerID   string
	ClientKey string
	Profile   string
	ImageRef  string
	Spec      json.RawMessage

	MaxDuration time.Duration
	IdleTimeout time.Duration
}

func (n NewSession) validate() error {
	if n.OwnerID == "" || n.ClientKey == "" || n.Profile == "" || n.ImageRef == "" ||
		!json.Valid(n.Spec) || n.MaxDuration <= 0 || n.IdleTimeout <= 0 {
		return ErrInvalid
	}
	return nil
}

// Limits are checked by Admit. Every field must be at least 1.
type Limits struct {
	// Capacity is the number of sessions that may hold a VM at once.
	Capacity int
	// PerOwner is the number of live sessions one owner may hold.
	PerOwner int
	// PerClient is the number of live sessions one client key may hold.
	PerClient int
}

func (l Limits) validate() error {
	if l.Capacity < 1 || l.PerOwner < 1 || l.PerClient < 1 {
		return ErrInvalid
	}
	return nil
}

// Claim is a session leased to one reconciler worker, with the columns the
// reconciler needs that Session does not show.
type Claim struct {
	Session
	Worker string

	// VMReleased is vm_released_at IS NOT NULL: the VM is confirmed gone and
	// the session no longer holds capacity.
	VMReleased bool
	// TerminatedAt is when the session reached STOPPED, FAILED or DELETED;
	// zero until then.
	TerminatedAt time.Time
	// Attempts counts consecutive failed steps, for backoff.
	Attempts int
}

// Result is the outcome of one reconcile step. Zero fields change nothing.
type Result struct {
	// Status is the new observed status; "" leaves it unchanged. Reaching
	// RUNNING sets started_at, and STOPPED, FAILED or DELETED set
	// terminated_at, each the first time only.
	Status Status
	// FailureReason is stored when not empty.
	FailureReason string
	// DesireDeleted is the reconciler's own decision to delete.
	DesireDeleted bool
	// ExpireReason, with DesireDeleted, says the session expired and why
	// ("idle", "max duration"). It is recorded as a PLAYGROUND_EXPIRED event.
	ExpireReason string
	// VMReleased records that the VM is confirmed gone, which frees its
	// capacity slot. Status DELETED needs it, now or from an earlier step.
	VMReleased bool
	// Failed counts this step as a failed attempt; otherwise the count resets.
	Failed bool
	// RetryAfter is when the session is next due. A session marked for
	// deletion is due at once whatever this says.
	RetryAfter time.Duration
}

// validate rejects a Result the schema would refuse.
func (r Result) validate(c Claim) error {
	switch {
	case r.Status != "" && !r.Status.valid():
		return ErrInvalid
	case r.Status == StatusDeleted && !r.VMReleased && !c.VMReleased:
		return ErrInvalid
	case r.RetryAfter < 0:
		return ErrInvalid
	case r.ExpireReason != "" && !r.DesireDeleted:
		return ErrInvalid
	}
	return nil
}

func (s Status) valid() bool {
	switch s {
	case StatusCreated, StatusProvisioning, StatusStarting, StatusRunning,
		StatusStopping, StatusStopped, StatusFailed, StatusDeleting, StatusDeleted:
		return true
	default:
		return false
	}
}

// terminal reports whether reaching s sets terminated_at.
func (s Status) terminal() bool {
	return s == StatusStopped || s == StatusFailed || s == StatusDeleted
}

// live reports whether a status counts toward quota while the desired state is
// RUNNING. It must match the predicate on the sessions_live_by_* partial
// indexes in the add_desired migration.
func (s Status) live() bool {
	switch s {
	case StatusCreated, StatusProvisioning, StatusStarting, StatusRunning:
		return true
	default:
		return false
	}
}

// canonicalID parses id and returns it in the canonical form the store hands
// out. Callers turn a false into ErrNotFound: a malformed ID is an ID that
// does not exist. Querying with the result, never the raw id, matters:
// uuid.Parse also accepts "urn:uuid:…", which PostgreSQL rejects (22P02).
func canonicalID(id string) (string, bool) {
	u, err := uuid.Parse(id)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// checkAdmit is the input check every Store's Admit runs before any work.
func checkAdmit(ctx context.Context, n NewSession, l Limits) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := n.validate(); err != nil {
		return err
	}
	return l.validate()
}

// Store keeps sessions. PostgreSQL is the real one; MemStore is the reference
// implementation for tests. Both must pass storetest.Run.
type Store interface {
	// Admit checks the limits and inserts the session as CREATED, atomically
	// with respect to every other Admit. It checks the owner's quota first
	// (so a double click gets *QuotaError with Yours and the ID), then
	// capacity (ErrCapacity), then the client's quota (*QuotaError with
	// ClientWide). Bad input gives ErrInvalid. A store that waits for a lock
	// gives ErrBusy when it waits too long.
	Admit(ctx context.Context, s NewSession, l Limits) (Session, error)

	// Get returns the session if ownerID owns it, else ErrNotFound. Sessions
	// already marked for deletion are returned too; callers decide visibility.
	Get(ctx context.Context, id, ownerID string) (Session, error)

	// RequestDelete sets the desired state to DELETED, which frees the
	// owner's and client's quota at once. Capacity is freed only when the
	// VM is gone. Idempotent. A session that is missing or owned by someone
	// else gives ErrNotFound.
	RequestDelete(ctx context.Context, id, ownerID string) error

	// Claim leases up to limit due sessions to worker for lease, oldest due
	// first. A session is due when it is not DELETED, its next attempt time
	// has come, and no other lease on it is current. No lock is held after
	// it returns: the provider work happens outside any transaction.
	Claim(ctx context.Context, worker string, lease time.Duration, limit int) ([]Claim, error)

	// Complete records a step's outcome and releases the lease. ErrLeaseLost
	// means another worker holds the session now; nothing was written.
	Complete(ctx context.Context, c Claim, r Result) error

	// TouchActivity records console input: it pushes the idle deadline to at
	// least now + idleFor. Callers throttle it. A missing ID is ErrNotFound.
	TouchActivity(ctx context.Context, id string, idleFor time.Duration) error

	// HoldingVM returns which of ids belong to sessions whose VM is not
	// released. The orphan sweep deletes any provider VM not among them;
	// IDs that are not session IDs at all are simply absent.
	HoldingVM(ctx context.Context, ids []string) (map[string]bool, error)

	// AcquireAttach takes the console lease for holder until now + lease.
	// The session must be ownerID's, RUNNING and not marked for deletion,
	// else ErrNotFound or ErrNotAttachable; a current lease held by another
	// holder is ErrAttached. An expired lease is taken over.
	AcquireAttach(ctx context.Context, id, ownerID, holder string, lease time.Duration) error

	// RenewAttach extends holder's lease to now + lease. ErrLeaseLost means
	// holder no longer has it, or the session is being deleted.
	RenewAttach(ctx context.Context, id, holder string, lease time.Duration) error

	// ReleaseAttach gives holder's lease up. It is idempotent: releasing a
	// lease holder does not have is nil.
	ReleaseAttach(ctx context.Context, id, holder string) error

	// PurgeDeletedSessions hard-deletes up to limit DELETED sessions that
	// ended more than olderThan ago, oldest first, and returns how many. Only
	// DELETED rows go: their VM is released, so the orphan sweep cannot
	// mistake a live VM for an orphan. Audit events refer to sessions by ID
	// with no foreign key, so they are unaffected. ErrBusy means another
	// replica is purging right now.
	PurgeDeletedSessions(ctx context.Context, olderThan time.Duration, limit int) (int, error)
}

// AuditPurger deletes old audit events. It is separate from Store because in
// PostgreSQL it needs the playground_retention role, which the API's own
// connection must not have.
type AuditPurger interface {
	// PurgeAuditEvents deletes up to limit events that occurred more than
	// olderThan ago, oldest first, and returns how many. ErrBusy means
	// another replica is purging right now.
	PurgeAuditEvents(ctx context.Context, olderThan time.Duration, limit int) (int, error)
}

func checkPurge(ctx context.Context, olderThan time.Duration, limit int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if olderThan <= 0 || limit < 1 {
		return ErrInvalid
	}
	return nil
}
