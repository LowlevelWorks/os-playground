// Package authtest is a fake OpenID Connect provider for tests, good enough
// for the real go-oidc client: discovery, signing keys, and a token endpoint
// that checks the PKCE verifier and signs ID tokens.
package authtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// IDP is the fake provider. A test plays the browser: it reads the authorize
// URL that sign-in redirects to, and registers with Grant what the provider
// answers for a code.
type IDP struct {
	t        testing.TB
	clientID string
	srv      *httptest.Server
	key      *rsa.PrivateKey

	mu     sync.Mutex
	grants map[string]grant // by code
}

// grant is what the provider returns for one code.
type grant struct {
	challenge string         // the code_challenge from the authorize request
	claims    map[string]any // ID token claims; iss, aud, exp, iat filled in if absent
	signer    *rsa.PrivateKey
}

// NewIDP starts a provider whose ID tokens are for clientID. It stops when
// the test ends.
func NewIDP(t testing.TB, clientID string) *IDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &IDP{t: t, clientID: clientID, key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", f.discovery)
	mux.HandleFunc("GET /keys", f.keys)
	mux.HandleFunc("POST /token", f.token)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

// Issuer is the provider's issuer URL.
func (f *IDP) Issuer() string { return f.srv.URL }

// Close stops the provider, as an outage would.
func (f *IDP) Close() { f.srv.Close() }

func (f *IDP) discovery(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"issuer":                                f.Issuer(),
		"authorization_endpoint":                f.Issuer() + "/authorize",
		"token_endpoint":                        f.Issuer() + "/token",
		"jwks_uri":                              f.Issuer() + "/keys",
		"id_token_signing_alg_values_supported": []string{"RS256"},
	})
}

func (f *IDP) keys(w http.ResponseWriter, _ *http.Request) {
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
		Key: &f.key.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig",
	}}})
}

// Grant registers the answer for code. challenge is the code_challenge the
// authorize request carried; the token request must prove it with the
// matching verifier, or the exchange fails, as at a real provider. iss, aud,
// iat and exp are filled in unless claims sets them.
func (f *IDP) Grant(code, challenge string, claims map[string]any) {
	f.GrantSignedBy(code, challenge, claims, f.key)
}

// GrantSignedBy is Grant with the ID token signed by key, which the
// provider does not publish unless it is its own.
func (f *IDP) GrantSignedBy(code, challenge string, claims map[string]any, signer *rsa.PrivateKey) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.grants[code] = grant{challenge: challenge, claims: claims, signer: signer}
}

func (f *IDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	g, ok := f.grants[r.PostForm.Get("code")]
	delete(f.grants, r.PostForm.Get("code")) // a code works once
	f.mu.Unlock()

	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "invalid_grant"})
		return
	}

	claims := map[string]any{
		"iss": f.Issuer(),
		"aud": f.clientID,
		"iat": time.Now().Unix(),
		"exp": time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range g.claims {
		claims[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "access",
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     f.sign(claims, g.signer),
	})
}

func (f *IDP) sign(claims map[string]any, key *rsa.PrivateKey) string {
	f.t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: "k1"}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		f.t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		f.t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	raw, err := obj.CompactSerialize()
	if err != nil {
		f.t.Fatal(err)
	}
	return raw
}
