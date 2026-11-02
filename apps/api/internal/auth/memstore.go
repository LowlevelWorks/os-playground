package auth

import (
	"context"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStore is the in-memory Store, for tests. Both stores pass the same
// contract (store_test.go).
type MemStore struct {
	mu       sync.Mutex
	now      func() time.Time
	users    map[[2]string]*memUser // by issuer and subject
	sessions map[string]memSession  // by token hash
	events   []MemEvent
}

type memUser struct {
	User
	disabled bool
}

type memSession struct {
	userID  string
	expires time.Time
}

// MemEvent is an audit event MemStore recorded.
type MemEvent struct {
	Type, RequestID, UserID string
}

var _ Store = (*MemStore)(nil)

// NewMemStore returns an empty store that reads the time from now.
func NewMemStore(now func() time.Time) *MemStore {
	return &MemStore{
		now:      now,
		users:    make(map[[2]string]*memUser),
		sessions: make(map[string]memSession),
	}
}

// Disable marks the user as unable to sign in, as an operator would.
func (m *MemStore) Disable(issuer, subject string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if u, ok := m.users[[2]string{issuer, subject}]; ok {
		u.disabled = true
	}
}

// Events returns the audit events recorded so far.
func (m *MemStore) Events() []MemEvent {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]MemEvent(nil), m.events...)
}

// Login implements Store.Login.
func (m *MemStore) Login(ctx context.Context, id Identity, ttl time.Duration, requestID string) (User, string, error) {
	if err := ctx.Err(); err != nil {
		return User{}, "", err
	}
	if !id.valid() || ttl <= 0 {
		return User{}, "", ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	key := [2]string{id.Issuer, id.Subject}
	u, ok := m.users[key]
	if !ok {
		u = &memUser{User: User{ID: uuid.NewString()}}
		m.users[key] = u
	}
	u.Email, u.Name = id.Email, id.Name
	if u.disabled {
		return User{}, "", ErrDisabled
	}
	token := newToken()
	hash, _ := tokenHash(token)
	m.sessions[string(hash)] = memSession{userID: u.ID, expires: m.now().Add(ttl)}
	m.events = append(m.events, MemEvent{EventLoggedIn, requestID, u.ID})
	return u.User, token, nil
}

// Lookup implements Store.Lookup.
func (m *MemStore) Lookup(ctx context.Context, token string) (User, error) {
	if err := ctx.Err(); err != nil {
		return User{}, err
	}
	hash, ok := tokenHash(token)
	if !ok {
		return User{}, ErrNoSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[string(hash)]
	if !ok || !m.now().Before(s.expires) {
		return User{}, ErrNoSession
	}
	for _, u := range m.users {
		if u.ID == s.userID {
			if u.disabled {
				return User{}, ErrNoSession
			}
			return u.User, nil
		}
	}
	return User{}, ErrNoSession
}

// Logout implements Store.Logout.
func (m *MemStore) Logout(ctx context.Context, token, requestID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	hash, ok := tokenHash(token)
	if !ok {
		return ErrNoSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.sessions[string(hash)]
	if !ok {
		return ErrNoSession
	}
	delete(m.sessions, string(hash))
	m.events = append(m.events, MemEvent{EventLoggedOut, requestID, s.userID})
	return nil
}

// PurgeExpired implements Store.PurgeExpired.
func (m *MemStore) PurgeExpired(ctx context.Context, limit int) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if limit < 1 {
		return 0, ErrInvalid
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	n := 0
	now := m.now()
	for h, s := range m.sessions {
		if n == limit {
			break
		}
		if !now.Before(s.expires) {
			delete(m.sessions, h)
			n++
		}
	}
	return n, nil
}
