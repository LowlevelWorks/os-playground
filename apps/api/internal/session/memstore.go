package session

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/binary"
	"fmt"
	"sync"
	"time"
)

// MemStore is the in-memory Store: the reference implementation the contract
// tests run against without a database. One mutex held across all of Admit
// stands in for PostgreSQL's admission advisory lock.
type MemStore struct {
	mu   sync.Mutex
	now  func() time.Time
	seq  uint64
	rows map[string]*row
}

var _ Store = (*MemStore)(nil)

// row is a stored session plus the columns Session does not show.
type row struct {
	s Session

	// vmReleasedAt is vm_released_at: zero while the row holds a VM or is
	// about to. Nothing sets it until the reconciler exists.
	vmReleasedAt time.Time

	// seq is insertion order, the tie-break when two rows share CreatedAt.
	seq uint64
}

// live is the quota predicate: desired RUNNING and a live status.
func (r *row) live() bool {
	return !r.s.DesiredDeleted && r.s.Status.live()
}

// holdsVM is the capacity predicate: vm_released_at IS NULL.
func (r *row) holdsVM() bool {
	return r.vmReleasedAt.IsZero()
}

// NewMemStore returns an empty store that reads the time from now.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{
		now:  now,
		rows: make(map[string]*row),
	}
}

// Admit implements Store.Admit. It holds m.mu for the whole count, check and
// insert, which is what the advisory lock does in PostgreSQL.
func (m *MemStore) Admit(ctx context.Context, n NewSession, l Limits) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}

	if err := n.validate(); err != nil {
		return Session{}, err
	}

	if err := l.validate(); err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	var (
		holding, ownerLive, clientLive int
		oldest                         *row
	)

	for _, r := range m.rows {
		if r.holdsVM() {
			holding++
		}

		if !r.live() {
			continue
		}

		if r.s.ClientKey == n.ClientKey {
			clientLive++
		}

		if sameOwner(r.s.OwnerID, n.OwnerID) {
			ownerLive++
			if oldest == nil || older(r, oldest) {
				oldest = r
			}
		}
	}

	yours := func() *QuotaError {
		if oldest == nil {
			return &QuotaError{}
		}

		return &QuotaError{
			Yours: true,
			ID:    oldest.s.ID,
		}
	}

	switch {
	case ownerLive >= l.PerOwner:
		return Session{}, yours()
	case holding >= l.Capacity:
		return Session{}, ErrCapacity
	case clientLive >= l.PerClient:
		q := yours()
		q.ClientWide = true
		return Session{}, q
	}

	// PostgreSQL stores microseconds; match it so both stores return the same.
	now := m.now().UTC().Truncate(time.Microsecond)
	m.seq++

	r := &row{
		s: Session{
			ID:            newID(now),
			OwnerID:       n.OwnerID,
			ClientKey:     n.ClientKey,
			Profile:       n.Profile,
			ImageRef:      n.ImageRef,
			Spec:          bytes.Clone(n.Spec),
			Status:        StatusCreated,
			CreatedAt:     now,
			ExpiresAt:     now.Add(n.MaxDuration),
			IdleExpiresAt: now.Add(n.IdleTimeout),
		},
		seq: m.seq,
	}
	m.rows[r.s.ID] = r
	return r.copy(), nil
}

// Get implements Store.Get.
func (m *MemStore) Get(ctx context.Context, id, ownerID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.owned(id, ownerID)
	if !ok {
		return Session{}, ErrNotFound
	}
	return r.copy(), nil
}

// RequestDelete implements Store.RequestDelete. It only sets DesiredDeleted:
// the VM, and so the capacity slot, is released later by the reconciler.
func (m *MemStore) RequestDelete(ctx context.Context, id, ownerID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.owned(id, ownerID)
	if !ok {
		return ErrNotFound
	}

	r.s.DesiredDeleted = true
	return nil
}

// owned returns the row if ownerID owns it. A missing row and someone else's
// row look the same to the caller. m.mu must be held.
func (m *MemStore) owned(id, ownerID string) (*row, bool) {
	r, ok := m.rows[id]
	if !ok || !sameOwner(r.s.OwnerID, ownerID) {
		return nil, false
	}
	return r, true
}

// copy returns the session with its own Spec, so a caller cannot change the
// store through the returned value.
func (r *row) copy() Session {
	s := r.s
	s.Spec = bytes.Clone(r.s.Spec)
	return s
}

// older orders rows by CreatedAt, then insertion order.
func older(a, b *row) bool {
	if !a.s.CreatedAt.Equal(b.s.CreatedAt) {
		return a.s.CreatedAt.Before(b.s.CreatedAt)
	}

	return a.seq < b.seq
}

// sameOwner compares in constant time, as owns does. The empty owner
// owns nothing.
func sameOwner(stored, given string) bool {
	return given != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(given)) == 1
}

// newID returns a UUIDv7 in its text form, the same shape PostgreSQL's
// uuidv7() gives. Callers must not depend on the format.
func newID(now time.Time) string {
	var b [16]byte
	var ms [8]byte
	binary.BigEndian.PutUint64(ms[:], uint64(now.UnixMilli()))
	copy(b[:6], ms[2:])
	_, _ = rand.Read(b[6:]) // never fails since Go 1.24
	b[6] = b[6]&0x0f | 0x70 // version 7
	b[8] = b[8]&0x3f | 0x80 // RFC 9562 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}
