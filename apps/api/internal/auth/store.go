// Package auth signs users in with OpenID Connect (Google) and keeps their
// login sessions. The API is the web app's backend for login: the browser never sees an OIDC token, only an HttpOnly
// cookie holding a random login-session token.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"
)

var (
	// ErrNoSession means the request carries no valid login session: no
	// cookie, an unknown or expired token, or a disabled user.
	ErrNoSession = errors.New("auth: not signed in")

	// ErrDisabled means the account exists but may not sign in.
	ErrDisabled = errors.New("auth: account disabled")

	// ErrInvalid means a malformed request to the store: a caller's bug.
	ErrInvalid = errors.New("auth: invalid store request")
)

// Audit event types, in the same audit_events table as session events.
// They carry the user's id only: never an email, a name or a token.
const (
	EventLoggedIn  = "USER_LOGGED_IN"
	EventLoggedOut = "USER_LOGGED_OUT"
)

// Identity is who the identity provider says signed in. Issuer and Subject
// identify the person; Email and Name are for display and may change.
type Identity struct {
	Issuer  string
	Subject string
	Email   string // empty unless the provider verified it
	Name    string
}

// User is a signed-in user as the API shows it.
type User struct {
	ID    string `json:"id"`
	Email string `json:"email,omitempty"`
	Name  string `json:"name,omitempty"`
}

// Store keeps users and their login sessions.
type Store interface {
	// Login records the identity (creating the user the first time, and
	// refreshing email and name after), creates a login session lasting ttl,
	// and writes a USER_LOGGED_IN audit event, all in one transaction. It
	// returns the user and the session token to put in the cookie. A
	// disabled user gets ErrDisabled and no session.
	Login(ctx context.Context, id Identity, ttl time.Duration, requestID string) (User, string, error)

	// Lookup returns the user a session token belongs to, or ErrNoSession.
	Lookup(ctx context.Context, token string) (User, error)

	// Logout ends the session and writes a USER_LOGGED_OUT event. An unknown
	// token is ErrNoSession.
	Logout(ctx context.Context, token, requestID string) error

	// PurgeExpired deletes up to limit expired login sessions and returns
	// how many.
	PurgeExpired(ctx context.Context, limit int) (int, error)
}

// tokenBytes of randomness make a session token: 256 bits, unguessable.
const tokenBytes = 32

// tokenLen is the length of an encoded token: base64url, no padding.
var tokenLen = base64.RawURLEncoding.EncodedLen(tokenBytes)

func newToken() string {
	b := make([]byte, tokenBytes)
	_, _ = rand.Read(b) // never fails on supported platforms
	return base64.RawURLEncoding.EncodeToString(b)
}

// tokenHash is what the store keeps instead of the token, so a copy of the
// table cannot be used to sign in. A malformed token has no hash: it cannot
// be a session's.
func tokenHash(token string) ([]byte, bool) {
	if len(token) != tokenLen {
		return nil, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(raw) != tokenBytes {
		return nil, false
	}
	sum := sha256.Sum256([]byte(token))
	return sum[:], true
}

func (id Identity) valid() bool {
	return id.Issuer != "" && id.Subject != ""
}
