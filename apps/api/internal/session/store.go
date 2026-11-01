package session

import (
	"context"
	"encoding/json"
	"errors"
	"time"

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
)

// Session is a stored session as the store returns it.
type Session struct {
	ID string

	// OwnerID is opaque: the anonymous owner token hash until OIDC lands, then
	// the user ID.
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

// Store keeps sessions. PostgreSQL is the real one; MemStore is the reference
// implementation for tests. Both must pass storetest.Run.
type Store interface {
	// Admit checks the limits and inserts the session as CREATED, atomically
	// with respect to every other Admit. It checks the owner's quota first
	// (so a double click gets *QuotaError with Yours and the ID), then
	// capacity (ErrCapacity), then the client's quota (*QuotaError with
	// ClientWide). Bad input gives ErrInvalid.
	Admit(ctx context.Context, s NewSession, l Limits) (Session, error)

	// Get returns the session if ownerID owns it, else ErrNotFound. Sessions
	// already marked for deletion are returned too; callers decide visibility.
	Get(ctx context.Context, id, ownerID string) (Session, error)

	// RequestDelete sets the desired state to DELETED, which frees the
	// owner's and client's quota at once. Capacity is freed only when the
	// VM is gone. Idempotent. A session that is missing or owned by someone
	// else gives ErrNotFound.
	RequestDelete(ctx context.Context, id, ownerID string) error
}
