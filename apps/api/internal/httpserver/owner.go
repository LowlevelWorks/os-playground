package httpserver

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/os-playground-api/internal/auth"
	"github.com/os-playground-api/internal/session"
)

// Auth is sign-in, as the server needs it: the OIDC routes, and who the
// login-session cookie on a request belongs to. *auth.Service implements it.
type Auth interface {
	// Authenticate returns the signed-in user. auth.ErrNoSession means none;
	// any other error is the store failing.
	Authenticate(r *http.Request) (auth.User, error)

	Login(w http.ResponseWriter, r *http.Request)
	Callback(w http.ResponseWriter, r *http.Request)
	Logout(w http.ResponseWriter, r *http.Request)
	Me(w http.ResponseWriter, r *http.Request)
}

// noAuth is used when sign-in is not configured: nobody is signed in, so
// nobody can start a machine, and the server says why.
type noAuth struct{}

func (noAuth) Authenticate(*http.Request) (auth.User, error) { return auth.User{}, auth.ErrNoSession }

func (noAuth) Login(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusServiceUnavailable, "sign-in is not configured on this server")
}

func (noAuth) Callback(w http.ResponseWriter, r *http.Request) { noAuth{}.Login(w, r) }

func (noAuth) Logout(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }

func (noAuth) Me(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusUnauthorized, "not signed in")
}

// wsProtocol is the only subprotocol the terminal speaks. Who is asking
// comes from the login cookie, which the browser sends with the upgrade.
const wsProtocol = "os-playground.v1"

// wsProtocolOffered reports whether the client offered exactly wsProtocol,
// so a client that does not speak the protocol never gets as far as Accept.
func wsProtocolOffered(r *http.Request) bool {
	n := 0
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			if strings.TrimSpace(p) != wsProtocol {
				return false
			}
			n++
		}
	}
	return n == 1
}

// ownedHandler is a handler that cannot run without a signed-in owner.
type ownedHandler func(w http.ResponseWriter, r *http.Request, o session.Owner)

// withOwner authenticates the request and passes its owner to next. Without
// a signed-in user next never runs: refuse answers instead. If the login
// store fails, the answer is 500, never refuse: an outage must not look like
// "you are signed out" or "no such VM".
func withOwner(a Auth, log *slog.Logger, refuse http.HandlerFunc, proxyHops int, next ownedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, err := a.Authenticate(r)
		switch {
		case errors.Is(err, auth.ErrNoSession):
			refuse(w, r)
			return
		case err != nil:
			log.Error("authenticate failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		next(w, r, session.Owner{UserID: u.ID, Client: clientKey(r, proxyHops)})
	}
}

// withWSProtocol refuses a WebSocket upgrade that does not offer exactly
// wsProtocol, with the same answer as a missing VM.
func withWSProtocol(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !wsProtocolOffered(r) {
			wsNotFound(w, r)
			return
		}
		next(w, r)
	}
}

// unauthorized refuses POST /playgrounds. It is the one route that says the
// caller is not signed in, since there is no VM whose existence that could
// reveal.
func unauthorized(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusUnauthorized, "sign in to start a playground")
}

// vmNotFound and wsNotFound are the only way a VM route says "not found",
// whatever the cause: no VM, another user's, or not signed in. One function
// per route keeps the answers identical, so none of them reveals which IDs
// exist.
func vmNotFound(w http.ResponseWriter, _ *http.Request) {
	writeError(w, http.StatusNotFound, "vm not found")
}

func wsNotFound(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "VM not found", http.StatusNotFound)
}
