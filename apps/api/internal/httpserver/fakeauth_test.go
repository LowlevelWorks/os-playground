package httpserver

import (
	"io"
	"log/slog"
	"net/http"

	"github.com/os-playground-api/internal/auth"
)

// Two signed-in users. The tokens stand in for login-session cookies; the
// IDs are what auth.Service would return for them.
const (
	userA = "user-a"
	userB = "user-b"

	sessionCookieName = "playground_session"
)

// fakeAuth signs in tokenA and tokenB through the session cookie, as
// auth.Service does after a real OIDC login. err, if set, is a store failure.
type fakeAuth struct{ err error }

func (f fakeAuth) Authenticate(r *http.Request) (auth.User, error) {
	if f.err != nil {
		return auth.User{}, f.err
	}
	c, err := r.Cookie(sessionCookieName)
	if err != nil {
		return auth.User{}, auth.ErrNoSession
	}
	switch c.Value {
	case tokenA:
		return auth.User{ID: userA}, nil
	case tokenB:
		return auth.User{ID: userB}, nil
	}
	return auth.User{}, auth.ErrNoSession
}

func (fakeAuth) Login(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "https://idp.test/authorize", http.StatusFound)
}

func (fakeAuth) Callback(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "http://localhost:3000/", http.StatusSeeOther)
}

func (fakeAuth) Logout(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func (f fakeAuth) Me(w http.ResponseWriter, r *http.Request) {
	u, err := f.Authenticate(r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// withSession adds token's login cookie to r; an empty token adds none.
func withSession(r *http.Request, token string) {
	if token != "" {
		r.AddCookie(&http.Cookie{Name: sessionCookieName, Value: token}) //nolint:gosec // G124: a request cookie in a test
	}
}

// sessionHeader is the Cookie header for token, for WebSocket dials.
func sessionHeader(token string) http.Header {
	h := http.Header{}
	if token != "" {
		h.Set("Cookie", sessionCookieName+"="+token)
	}
	return h
}

var discard = slog.New(slog.NewTextHandler(io.Discard, nil))
