package session

import (
	"bytes"
	"cmp"
	"context"
	"crypto/subtle"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is the in-memory Store: the reference implementation the contract
// tests run against without a database. One mutex held across all of Admit
// stands in for PostgreSQL's admission advisory lock.
type MemStore struct {
	mu    sync.Mutex
	now   func() time.Time
	seq   uint64
	rows  map[string]*row
	audit []AuditEvent
}

var _ Store = (*MemStore)(nil)

// row is a stored session plus the columns Session does not show.
type row struct {
	s Session

	// vmReleasedAt is vm_released_at: zero while the row holds a VM or is
	// about to.
	vmReleasedAt time.Time

	// The reconciler's columns, zero when unset.
	startedAt    time.Time
	terminatedAt time.Time
	lastActivity time.Time
	claimedBy    string
	claimUntil   time.Time
	attachHolder string
	attachUntil  time.Time
	nextAttempt  time.Time
	attempts     int

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
	if err := checkAdmit(ctx, n, l); err != nil {
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
			ID:            uuid.Must(uuid.NewV7()).String(),
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
		nextAttempt: now,
		seq:         m.seq,
	}
	m.rows[r.s.ID] = r
	m.record(now, createdEvent(ctx, r.s))
	return r.copy(), nil
}

// Get implements Store.Get.
func (m *MemStore) Get(ctx context.Context, id, ownerID string) (Session, error) {
	if err := ctx.Err(); err != nil {
		return Session{}, err
	}

	cid, ok := canonicalID(id)
	if !ok {
		return Session{}, ErrNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.owned(cid, ownerID)
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

	cid, ok := canonicalID(id)
	if !ok {
		return ErrNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.owned(cid, ownerID)
	if !ok {
		return ErrNotFound
	}

	if r.s.DesiredDeleted {
		return nil // idempotent: one event, no change
	}
	now := m.now().UTC().Truncate(time.Microsecond)
	r.s.DesiredDeleted = true
	r.nextAttempt = now
	m.record(now, deleteRequestedEvent(ctx, r.s.ID, r.s.OwnerID))
	return nil
}

// Claim implements Store.Claim.
func (m *MemStore) Claim(ctx context.Context, worker string, lease time.Duration, limit int) ([]Claim, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if worker == "" || lease <= 0 || limit < 1 {
		return nil, ErrInvalid
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now().UTC()
	var due []*row
	for _, r := range m.rows {
		if r.s.Status != StatusDeleted && !r.nextAttempt.After(now) &&
			(r.claimUntil.IsZero() || r.claimUntil.Before(now)) {
			due = append(due, r)
		}
	}
	slices.SortFunc(due, func(a, b *row) int {
		if c := a.nextAttempt.Compare(b.nextAttempt); c != 0 {
			return c
		}
		return cmp.Compare(a.seq, b.seq)
	})
	if len(due) > limit {
		due = due[:limit]
	}

	out := make([]Claim, 0, len(due))
	for _, r := range due {
		r.claimedBy = worker
		r.claimUntil = now.Add(lease).Truncate(time.Microsecond)
		out = append(out, Claim{
			Session:      r.copy(),
			Worker:       worker,
			VMReleased:   !r.vmReleasedAt.IsZero(),
			TerminatedAt: r.terminatedAt,
			Attempts:     r.attempts,
		})
	}
	return out, nil
}

// Complete implements Store.Complete.
func (m *MemStore) Complete(ctx context.Context, c Claim, res Result) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := res.validate(c); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rows[c.ID]
	if !ok || c.Worker == "" || r.claimedBy != c.Worker {
		return ErrLeaseLost
	}

	now := m.now().UTC().Truncate(time.Microsecond)
	for _, e := range completeEvents(r.s.ID, r.s.OwnerID, r.s.Status, r.s.DesiredDeleted, res) {
		m.record(now, e)
	}
	if res.Status != "" {
		r.s.Status = res.Status
		if res.Status == StatusRunning && r.startedAt.IsZero() {
			r.startedAt = now
		}
		if res.Status.terminal() && r.terminatedAt.IsZero() {
			r.terminatedAt = now
		}
	}
	if res.FailureReason != "" {
		r.s.FailureReason = res.FailureReason
	}
	if res.DesireDeleted {
		r.s.DesiredDeleted = true
	}
	if res.VMReleased && r.vmReleasedAt.IsZero() {
		r.vmReleasedAt = now
	}
	if res.Failed {
		r.attempts++
	} else {
		r.attempts = 0
	}
	r.nextAttempt = now.Add(res.RetryAfter)
	if r.s.DesiredDeleted {
		r.nextAttempt = now
	}
	r.claimedBy, r.claimUntil = "", time.Time{}
	return nil
}

// TouchActivity implements Store.TouchActivity.
func (m *MemStore) TouchActivity(ctx context.Context, id string, idleFor time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if idleFor <= 0 {
		return ErrInvalid
	}
	cid, ok := canonicalID(id)
	if !ok {
		return ErrNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rows[cid]
	if !ok {
		return ErrNotFound
	}
	now := m.now().UTC().Truncate(time.Microsecond)
	r.lastActivity = now
	if until := now.Add(idleFor); until.After(r.s.IdleExpiresAt) {
		r.s.IdleExpiresAt = until
	}
	return nil
}

// HoldingVM implements Store.HoldingVM.
func (m *MemStore) HoldingVM(ctx context.Context, ids []string) (map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make(map[string]bool)
	for _, id := range ids {
		cid, ok := canonicalID(id)
		if !ok || cid != id {
			continue // a provider VM's name is the canonical ID, or not ours
		}
		if r, ok := m.rows[cid]; ok && r.holdsVM() {
			out[id] = true
		}
	}
	return out, nil
}

// AcquireAttach implements Store.AcquireAttach.
func (m *MemStore) AcquireAttach(ctx context.Context, id, ownerID, holder string, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if holder == "" || lease <= 0 {
		return ErrInvalid
	}
	cid, ok := canonicalID(id)
	if !ok {
		return ErrNotFound
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.owned(cid, ownerID)
	switch {
	case !ok:
		return ErrNotFound
	case r.s.DesiredDeleted || r.s.Status != StatusRunning:
		return ErrNotAttachable
	}
	now := m.now().UTC()
	if r.attachHolder != "" && r.attachHolder != holder && !r.attachUntil.Before(now) {
		return ErrAttached
	}
	r.attachHolder, r.attachUntil = holder, now.Add(lease).Truncate(time.Microsecond)
	return nil
}

// RenewAttach implements Store.RenewAttach.
func (m *MemStore) RenewAttach(ctx context.Context, id, holder string, lease time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if holder == "" || lease <= 0 {
		return ErrInvalid
	}
	cid, _ := canonicalID(id)

	m.mu.Lock()
	defer m.mu.Unlock()

	r, ok := m.rows[cid]
	if !ok || r.attachHolder != holder || r.s.DesiredDeleted {
		return ErrLeaseLost
	}
	r.attachUntil = m.now().UTC().Add(lease).Truncate(time.Microsecond)
	return nil
}

// ReleaseAttach implements Store.ReleaseAttach.
func (m *MemStore) ReleaseAttach(ctx context.Context, id, holder string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	cid, _ := canonicalID(id)

	m.mu.Lock()
	defer m.mu.Unlock()

	if r, ok := m.rows[cid]; ok && holder != "" && r.attachHolder == holder {
		r.attachHolder, r.attachUntil = "", time.Time{}
	}
	return nil
}

// AuditEvents returns the events recorded for a session, oldest first. It
// is for tests and is not part of Store.
func (m *MemStore) AuditEvents(ctx context.Context, sessionID string) ([]AuditEvent, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []AuditEvent
	for _, e := range m.audit {
		if e.SessionID == sessionID {
			e.Metadata = maps.Clone(e.Metadata)
			out = append(out, e)
		}
	}
	return out, nil
}

// record appends an audit event. m.mu must be held.
func (m *MemStore) record(at time.Time, e AuditEvent) {
	e.OccurredAt = at
	if e.Metadata == nil {
		e.Metadata = map[string]string{}
	}
	m.audit = append(m.audit, e)
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

// sameOwner compares in constant time, so timing does not leak a hash. The
// empty owner owns nothing.
func sameOwner(stored, given string) bool {
	return given != "" && subtle.ConstantTimeCompare([]byte(stored), []byte(given)) == 1
}
