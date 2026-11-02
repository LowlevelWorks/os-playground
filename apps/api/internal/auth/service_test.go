package auth

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/os-playground-api/internal/auth/authtest"
)

const (
	testClientID = "client-123"
	webOrigin    = "http://localhost:3000"
	callbackPath = "/api/v1/auth/callback"
)

type rig struct {
	t     *testing.T
	idp   *authtest.IDP
	store *MemStore
	s     *Service
}

func newRig(t *testing.T, publicURL string) *rig {
	t.Helper()
	idp := authtest.NewIDP(t, testClientID)
	store := NewMemStore(time.Now)
	s, err := New(Config{
		Issuer: idp.Issuer(), ClientID: testClientID, ClientSecret: "secret",
		PublicURL: publicURL, CallbackPath: callbackPath,
		AllowedOrigins: []string{webOrigin, "https://play.example.com"},
		SessionTTL:     24 * time.Hour,
		Store:          store,
		RequestID:      func(context.Context) string { return "req-1" },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &rig{t: t, idp: idp, store: store, s: s}
}

// attempt is a login in progress, as the browser holds it.
type attempt struct {
	cookies   []*http.Cookie
	state     string
	nonce     string
	challenge string
	authorize *url.URL
}

// login calls Login and follows nothing: it returns what the browser would
// carry to the provider.
func (r *rig) login(returnTo string) attempt {
	r.t.Helper()
	target := "/api/v1/auth/login"
	if returnTo != "" {
		target += "?return_to=" + url.QueryEscape(returnTo)
	}
	rec := httptest.NewRecorder()
	r.s.Login(rec, httptest.NewRequest("GET", target, nil))
	if rec.Code != http.StatusFound {
		r.t.Fatalf("Login = %d %s, want 302", rec.Code, rec.Body)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		r.t.Fatal(err)
	}
	q := loc.Query()
	return attempt{
		cookies: rec.Result().Cookies(),
		state:   q.Get("state"), nonce: q.Get("nonce"), challenge: q.Get("code_challenge"),
		authorize: loc,
	}
}

// callback plays the provider's redirect back, with the given query.
func (r *rig) callback(a attempt, query url.Values) *httptest.ResponseRecorder {
	r.t.Helper()
	req := httptest.NewRequest("GET", callbackPath+"?"+query.Encode(), nil)
	for _, c := range a.cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.s.Callback(rec, req)
	return rec
}

// signIn runs the whole flow for subject and returns the callback response.
func (r *rig) signIn(subject string, extra map[string]any) *httptest.ResponseRecorder {
	r.t.Helper()
	a := r.login(webOrigin + "/play?x=1")
	claims := map[string]any{"sub": subject, "nonce": a.nonce, "email": "ada@example.com", "email_verified": true, "name": "Ada"}
	for k, v := range extra {
		claims[k] = v
	}
	r.idp.Grant("code-1", a.challenge, claims)
	return r.callback(a, url.Values{"state": {a.state}, "code": {"code-1"}})
}

func cookie(rec *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range rec.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func (r *rig) me(c *http.Cookie) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	if c != nil {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	r.s.Me(rec, req)
	return rec
}

func wantOutcome(t *testing.T, rec *httptest.ResponseRecorder, wantPrefix, outcome string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, wantPrefix) {
		t.Errorf("Location = %q, want it to start %q", loc, wantPrefix)
	}
	u, _ := url.Parse(loc)
	if got := u.Query().Get("login"); got != outcome {
		t.Errorf("login = %q, want %q (Location %s)", got, outcome, loc)
	}
	if c := cookie(rec, sessionCookie); c != nil && c.MaxAge >= 0 {
		t.Errorf("a failed login set a session cookie: %+v", c)
	}
}

func TestNewReportsEveryProblem(t *testing.T) {
	_, err := New(Config{PublicURL: "https://x.example/api"})
	if err == nil {
		t.Fatal("New(Config{}) = nil")
	}
	for _, want := range []string{"Issuer", "ClientID", "ClientSecret", "CallbackPath", "PublicURL", "AllowedOrigins", "SessionTTL", "Store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoginRedirectsWithStateNoncePKCE(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	a := r.login(webOrigin + "/")

	q := a.authorize.Query()
	if a.authorize.Scheme+"://"+a.authorize.Host+a.authorize.Path != r.idp.Issuer()+"/authorize" {
		t.Errorf("redirect to %s, want the provider's authorize endpoint", a.authorize)
	}
	for k, want := range map[string]string{
		"client_id": testClientID, "response_type": "code", "code_challenge_method": "S256",
		"redirect_uri": "http://localhost:8080" + callbackPath, "prompt": "select_account",
	} {
		if q.Get(k) != want {
			t.Errorf("%s = %q, want %q", k, q.Get(k), want)
		}
	}
	if !strings.Contains(q.Get("scope"), "openid") || a.state == "" || a.nonce == "" || a.challenge == "" {
		t.Errorf("authorize query = %v, want openid scope, state, nonce and a PKCE challenge", q)
	}
	c := cookie(httptestFromCookies(a.cookies), loginCookie)
	if c == nil || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge <= 0 {
		t.Fatalf("login cookie = %+v, want HttpOnly, SameSite=Lax, short-lived", c)
	}
	if strings.Contains(c.Value, a.authorize.Query().Get("code_challenge")) {
		t.Error("the login cookie should hold the verifier, not the challenge")
	}
}

// httptestFromCookies wraps cookies so cookie() can search them.
func httptestFromCookies(cs []*http.Cookie) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	for _, c := range cs {
		http.SetCookie(rec, c)
	}
	return rec
}

func TestFullSignInAndOut(t *testing.T) {
	r := newRig(t, "http://localhost:8080")

	rec := r.signIn("google-sub-1", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != webOrigin+"/play?x=1" {
		t.Fatalf("callback = %d to %q, want 303 back to the page", rec.Code, rec.Header().Get("Location"))
	}
	sc := cookie(rec, sessionCookie)
	if sc == nil || !sc.HttpOnly || sc.Secure || sc.SameSite != http.SameSiteLaxMode || sc.Path != "/" || sc.MaxAge != 86400 {
		t.Fatalf("session cookie = %+v", sc)
	}
	if lc := cookie(rec, loginCookie); lc == nil || lc.MaxAge >= 0 {
		t.Errorf("the login cookie was not cleared: %+v", lc)
	}

	me := r.me(sc)
	var u User
	if me.Code != http.StatusOK || json.Unmarshal(me.Body.Bytes(), &u) != nil || u.Email != "ada@example.com" || u.Name != "Ada" || u.ID == "" {
		t.Fatalf("Me = %d %s", me.Code, me.Body)
	}
	if me.Header().Get("Cache-Control") != "no-store" {
		t.Error("Me is cacheable")
	}

	// The same person signing in again is the same user.
	again := r.signIn("google-sub-1", map[string]any{"name": "Ada L."})
	var u2 User
	_ = json.Unmarshal(r.me(cookie(again, sessionCookie)).Body.Bytes(), &u2)
	if u2.ID != u.ID || u2.Name != "Ada L." {
		t.Errorf("second sign-in = %+v, want user %s with the new name", u2, u.ID)
	}

	out := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/auth/logout", nil)
	req.AddCookie(sc)
	r.s.Logout(out, req)
	if out.Code != http.StatusNoContent {
		t.Fatalf("Logout = %d", out.Code)
	}
	if c := cookie(out, sessionCookie); c == nil || c.MaxAge >= 0 {
		t.Errorf("Logout did not clear the cookie: %+v", c)
	}
	if code := r.me(sc).Code; code != http.StatusUnauthorized {
		t.Errorf("Me with the old cookie = %d, want 401", code)
	}

	events := r.store.Events()
	if len(events) != 3 || events[0].Type != EventLoggedIn || events[2].Type != EventLoggedOut ||
		events[0].UserID != u.ID || events[0].RequestID != "req-1" {
		t.Errorf("audit events = %+v", events)
	}
}

func TestCallbackFailures(t *testing.T) {
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		outcome string
		run     func(r *rig, a attempt) *httptest.ResponseRecorder
	}{
		{"wrong state", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", a.challenge, map[string]any{"sub": "s", "nonce": a.nonce})
			return r.callback(a, url.Values{"state": {"forged"}, "code": {"c"}})
		}},
		{"user cancelled", "cancelled", func(r *rig, a attempt) *httptest.ResponseRecorder {
			return r.callback(a, url.Values{"state": {a.state}, "error": {"access_denied"}})
		}},
		{"provider error", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			return r.callback(a, url.Values{"state": {a.state}, "error": {"server_error"}})
		}},
		{"no code", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			return r.callback(a, url.Values{"state": {a.state}})
		}},
		{"unknown code", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			return r.callback(a, url.Values{"state": {a.state}, "code": {"never-issued"}})
		}},
		{"PKCE verifier does not match", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", "not-the-challenge", map[string]any{"sub": "s", "nonce": a.nonce})
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
		{"wrong nonce", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", a.challenge, map[string]any{"sub": "s", "nonce": "replayed"})
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
		{"token for another client", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", a.challenge, map[string]any{"sub": "s", "nonce": a.nonce, "aud": "someone-else"})
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
		{"token from another issuer", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", a.challenge, map[string]any{"sub": "s", "nonce": a.nonce, "iss": "https://evil.example"})
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
		{"expired token", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.Grant("c", a.challenge, map[string]any{"sub": "s", "nonce": a.nonce, "exp": time.Now().Add(-time.Hour).Unix()})
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
		{"token signed by another key", "failed", func(r *rig, a attempt) *httptest.ResponseRecorder {
			r.idp.GrantSignedBy("c", a.challenge, map[string]any{"sub": "s", "nonce": a.nonce}, otherKey)
			return r.callback(a, url.Values{"state": {a.state}, "code": {"c"}})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newRig(t, "http://localhost:8080")
			a := r.login(webOrigin + "/here")
			wantOutcome(t, tt.run(r, a), webOrigin+"/here", tt.outcome)
			if n := len(r.store.Events()); n != 0 {
				t.Errorf("%d audit events, want none", n)
			}
		})
	}
}

func TestCallbackWithoutALoginInProgress(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	wantOutcome(t, r.callback(attempt{}, url.Values{"state": {"x"}, "code": {"y"}}), webOrigin+"/", "failed")
}

func TestDisabledUserCannotSignIn(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	r.signIn("sub-d", nil)
	r.store.Disable(r.idp.Issuer(), "sub-d")

	a := r.login("")
	r.idp.Grant("c", a.challenge, map[string]any{"sub": "sub-d", "nonce": a.nonce})
	wantOutcome(t, r.callback(a, url.Values{"state": {a.state}, "code": {"c"}}), webOrigin+"/", "disabled")
}

// An email the provider has not verified is never stored: it would let
// someone claim to be someone else on screen.
func TestUnverifiedEmailIsNotKept(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	rec := r.signIn("sub-u", map[string]any{"email_verified": false})
	var u User
	_ = json.Unmarshal(r.me(cookie(rec, sessionCookie)).Body.Bytes(), &u)
	if u.Email != "" {
		t.Errorf("Email = %q, want none", u.Email)
	}
}

// Login can only return to the web app, never to an attacker's site.
func TestReturnToMustBeTheWebApp(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	for raw, want := range map[string]string{
		"":                                   webOrigin + "/",
		webOrigin + "/a?b=c":                 webOrigin + "/a?b=c",
		"https://PLAY.example.com/x":         "https://PLAY.example.com/x",
		"https://evil.example/":              webOrigin + "/",
		"//evil.example/":                    webOrigin + "/",
		"/relative":                          webOrigin + "/",
		"javascript:alert(1)":                webOrigin + "/",
		"http://user@localhost:3000/":        webOrigin + "/",
		"http://localhost:3000.evil.example": webOrigin + "/",
		webOrigin + "/" + strings.Repeat("a", maxReturnTo): webOrigin + "/",
	} {
		if got := r.s.returnTo(raw); got != want {
			t.Errorf("returnTo(%q) = %q, want %q", raw, got, want)
		}
	}
}

// Over https every cookie is Secure and __Host- prefixed, so no other host
// can set or overwrite it.
func TestHTTPSCookiesAreHostPrefixed(t *testing.T) {
	r := newRig(t, "https://play.example.com")
	rec := r.signIn("sub-h", nil)
	sc := cookie(rec, "__Host-"+sessionCookie)
	if sc == nil || !sc.Secure || sc.Path != "/" || sc.Domain != "" {
		t.Fatalf("session cookie = %+v, want __Host-, Secure, Path=/", sc)
	}
	if r.me(sc).Code != http.StatusOK {
		t.Error("the __Host- cookie does not authenticate")
	}
	unprefixed := &http.Cookie{Name: sessionCookie, Value: sc.Value} //nolint:gosec // G124: a request cookie in a test
	if r.me(unprefixed).Code != http.StatusUnauthorized {
		t.Error("an unprefixed cookie was accepted over https")
	}
}

// The provider being down at startup must not stop the API; login works
// once it is back.
func TestDiscoveryFailureIsRetried(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	r.idp.Close()

	rec := httptest.NewRecorder()
	r.s.Login(rec, httptest.NewRequest("GET", "/api/v1/auth/login", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("Login with the provider down = %d, want 503", rec.Code)
	}

	idp := authtest.NewIDP(t, testClientID) // the provider is back, at a new address
	r.s.issuer = idp.Issuer()
	r.idp = idp
	if a := r.login(""); a.state == "" {
		t.Fatal("Login after the provider came back did not redirect")
	}
}

func TestMeWithoutACookieIs401(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	//nolint:gosec // G124: request cookies in a test
	for _, c := range []*http.Cookie{nil, {Name: sessionCookie, Value: "garbage"}, {Name: sessionCookie, Value: newToken()}} {
		if code := r.me(c).Code; code != http.StatusUnauthorized {
			t.Errorf("Me with %v = %d, want 401", c, code)
		}
	}
}

func TestLogoutWithoutASessionIs204(t *testing.T) {
	r := newRig(t, "http://localhost:8080")
	rec := httptest.NewRecorder()
	r.s.Logout(rec, httptest.NewRequest("POST", "/api/v1/auth/logout", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("Logout = %d, want 204", rec.Code)
	}
}
