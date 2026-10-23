package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"

	"github.com/os-playground-api/internal/session"
)

// An owner token is 32 random bytes the browser makes for itself, base64url
// without padding. It proves only "I am the browser that created this VM", so
// the server needs no account to check it: it keeps the token's SHA-256 and
// compares that. Neither the token nor its hash is ever logged.
const tokenLen = 43

// wsProtocol is the only subprotocol the terminal speaks. Browsers cannot set
// headers on a WebSocket, so the token rides beside it as "token.<token>";
// the server answers with wsProtocol alone, so the token is never echoed.
const (
	wsProtocol    = "os-playground.v1"
	wsTokenPrefix = "token."
)

func validToken(s string) bool {
	if len(s) != tokenLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if !isTokenChar(s[i]) {
			return false
		}
	}
	return true
}

// isTokenChar reports whether c is in the base64url alphabet.
func isTokenChar(c byte) bool {
	return 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || c == '-' || c == '_'
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// bearerToken reads "Authorization: Bearer <token>". A missing header, a
// repeated one, another scheme or a malformed token all fail.
func bearerToken(r *http.Request) (string, bool) {
	values := r.Header.Values("Authorization")
	if len(values) != 1 {
		return "", false
	}
	scheme, token, ok := strings.Cut(values[0], " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || !validToken(token) {
		return "", false
	}
	return token, true
}

// wsToken reads the token from Sec-WebSocket-Protocol. The offer must be
// exactly wsProtocol and one token entry, in any order; anything else fails,
// so a client that does not speak the protocol never gets as far as Accept.
func wsToken(r *http.Request) (string, bool) {
	var token string
	protocols, tokens := 0, 0
	for _, v := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, p := range strings.Split(v, ",") {
			switch p = strings.TrimSpace(p); {
			case p == wsProtocol:
				protocols++
			case strings.HasPrefix(p, wsTokenPrefix):
				tokens++
				token = strings.TrimPrefix(p, wsTokenPrefix)
			default:
				return "", false
			}
		}
	}
	if protocols != 1 || tokens != 1 || !validToken(token) {
		return "", false
	}
	return token, true
}

// owner builds the session.Owner for a request whose token has been read.
func owner(r *http.Request, token string, proxyHops int) session.Owner {
	return session.Owner{Hash: hashToken(token), Client: clientKey(r, proxyHops)}
}

// ownedHandler is a handler that cannot run without a verified owner.
type ownedHandler func(w http.ResponseWriter, r *http.Request, o session.Owner)

// withOwner reads the token with read and passes the owner to next. Without a
// valid token next never runs: refuse answers instead. Routes differ only in
// where the token is and how a bad one is refused.
func withOwner(read func(*http.Request) (string, bool), refuse http.HandlerFunc, proxyHops int, next ownedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := read(r)
		if !ok {
			refuse(w, r)
			return
		}
		next(w, r, owner(r, token, proxyHops))
	}
}

// unauthorized refuses POST /vms. It is the one route that says a token is
// wrong, since there is no VM whose existence that could reveal.
func unauthorized(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeError(w, http.StatusUnauthorized, "missing or invalid token")
}

// vmNotFound and wsNotFound are the only way a VM route says "not found",
// whatever the cause: no VM, another owner's, or a bad token. One function
// per route keeps the answers identical, so none of them reveals which IDs
// exist.
func vmNotFound(w http.ResponseWriter, r *http.Request) {
	writeError(w, http.StatusNotFound, "vm not found")
}

func wsNotFound(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "VM not found", http.StatusNotFound)
}
