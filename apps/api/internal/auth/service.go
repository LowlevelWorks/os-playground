package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

// GoogleIssuer is Google's OIDC issuer, the default provider.
const GoogleIssuer = "https://accounts.google.com"

const (
	sessionCookie = "playground_session"
	loginCookie   = "playground_login"

	// loginTTL bounds how long the user may spend at the provider.
	loginTTL = 10 * time.Minute
	// maxReturnTo keeps the login cookie well under the 4 KB cookie limit.
	maxReturnTo = 2048
	// discoveryTimeout bounds the HTTP calls to the provider.
	discoveryTimeout = 10 * time.Second
)

// Config configures a Service. Every field except RequestID, HTTPClient and
// Logger is required.
type Config struct {
	Issuer       string
	ClientID     string
	ClientSecret string

	// PublicURL is the API's origin as browsers reach it, such as
	// "https://play.example.com" or "http://localhost:8080". The provider
	// redirects to PublicURL + CallbackPath, which must be registered with
	// it. An https URL makes every cookie Secure and __Host- prefixed.
	PublicURL    string
	CallbackPath string

	// AllowedOrigins are the web app's origins: the only places a login may
	// return to. The first is where it returns when none is given.
	AllowedOrigins []string

	// SessionTTL is how long a login lasts.
	SessionTTL time.Duration

	Store      Store
	RequestID  func(context.Context) string // for audit events; nil records none
	HTTPClient *http.Client                 // talks to the provider; nil has a 10s timeout
	Logger     *slog.Logger                 // nil discards
}

// Service runs the OIDC authorization code flow with PKCE, and turns its
// result into a login session held in an HttpOnly cookie.
type Service struct {
	issuer        string
	clientID      string
	clientSecret  string
	redirectURL   string
	secure        bool
	origins       map[string]bool
	defaultReturn string
	ttl           time.Duration
	store         Store
	requestID     func(context.Context) string
	client        *http.Client
	log           *slog.Logger

	// Discovered on first use, so the API starts even while the provider
	// is unreachable; a failure is retried at the next login.
	mu       sync.Mutex
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

// New checks the configuration.
func New(cfg Config) (*Service, error) {
	var errs []error
	for name, v := range map[string]string{
		"Issuer": cfg.Issuer, "ClientID": cfg.ClientID, "ClientSecret": cfg.ClientSecret, "CallbackPath": cfg.CallbackPath,
	} {
		if v == "" {
			errs = append(errs, fmt.Errorf("auth: %s is required", name))
		}
	}
	public, err := url.Parse(cfg.PublicURL)
	if err != nil || (public.Scheme != "http" && public.Scheme != "https") || public.Host == "" ||
		(public.Path != "" && public.Path != "/") || public.RawQuery != "" || public.User != nil {
		errs = append(errs, fmt.Errorf("auth: PublicURL=%q: must be an origin such as https://play.example.com", cfg.PublicURL))
	}
	if len(cfg.AllowedOrigins) == 0 {
		errs = append(errs, errors.New("auth: AllowedOrigins is required"))
	}
	if cfg.SessionTTL <= 0 {
		errs = append(errs, fmt.Errorf("auth: SessionTTL=%s: must be greater than 0", cfg.SessionTTL))
	}
	if cfg.Store == nil {
		errs = append(errs, errors.New("auth: Store is required"))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	s := &Service{
		issuer:        cfg.Issuer,
		clientID:      cfg.ClientID,
		clientSecret:  cfg.ClientSecret,
		redirectURL:   strings.TrimSuffix(cfg.PublicURL, "/") + cfg.CallbackPath,
		secure:        public.Scheme == "https",
		origins:       make(map[string]bool, len(cfg.AllowedOrigins)),
		defaultReturn: strings.TrimSuffix(cfg.AllowedOrigins[0], "/") + "/",
		ttl:           cfg.SessionTTL,
		store:         cfg.Store,
		requestID:     cfg.RequestID,
		client:        cfg.HTTPClient,
		log:           cfg.Logger,
	}
	for _, o := range cfg.AllowedOrigins {
		s.origins[strings.ToLower(strings.TrimSuffix(o, "/"))] = true
	}
	if s.requestID == nil {
		s.requestID = func(context.Context) string { return "" }
	}
	if s.client == nil {
		s.client = &http.Client{Timeout: discoveryTimeout}
	}
	if s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return s, nil
}

// cookieName adds the __Host- prefix over https: the browser then accepts
// the cookie only if it is Secure, has Path=/ and no Domain, so no other
// host, not even a subdomain, can plant or overwrite it.
func (s *Service) cookieName(base string) string {
	if s.secure {
		return "__Host-" + base
	}
	return base
}

func (s *Service) setCookie(w http.ResponseWriter, name, value string, maxAge time.Duration) {
	// Secure follows PublicURL: config allows plain http for localhost only.
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: Secure is s.secure, false only for http://localhost
		Name:     s.cookieName(name),
		Value:    value,
		Path:     "/",
		MaxAge:   int(maxAge / time.Second),
		HttpOnly: true,
		Secure:   s.secure,
		// Lax: sent on the top-level redirect back from the provider, never
		// on a cross-site POST or DELETE.
		SameSite: http.SameSiteLaxMode,
	})
}

func (s *Service) clearCookie(w http.ResponseWriter, name string) {
	http.SetCookie(w, &http.Cookie{ //nolint:gosec // G124: as in setCookie
		Name: s.cookieName(name), Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: s.secure, SameSite: http.SameSiteLaxMode,
	})
}

// endpoints discovers the provider once. It uses a context without a
// deadline on purpose: go-oidc keeps it to fetch signing keys later, and
// the HTTP client's timeout bounds each call.
func (s *Service) endpoints() (*oauth2.Config, *oidc.IDTokenVerifier, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.oauth != nil {
		return s.oauth, s.verifier, nil
	}
	p, err := oidc.NewProvider(oidc.ClientContext(context.Background(), s.client), s.issuer)
	if err != nil {
		return nil, nil, fmt.Errorf("discover %s: %w", s.issuer, err)
	}
	s.oauth = &oauth2.Config{
		ClientID:     s.clientID,
		ClientSecret: s.clientSecret,
		Endpoint:     p.Endpoint(),
		RedirectURL:  s.redirectURL,
		Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
	}
	s.verifier = p.Verifier(&oidc.Config{ClientID: s.clientID})
	return s.oauth, s.verifier, nil
}

// loginState is what the login cookie carries from Login to Callback. It
// needs no signature: it is HttpOnly, so scripts cannot read it, and the
// state check ties it to the provider's redirect.
type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	ReturnTo string `json:"r"`
}

// Login handles GET /auth/login?return_to=<url>: it remembers the attempt in
// a short-lived cookie and redirects to the provider.
func (s *Service) Login(w http.ResponseWriter, r *http.Request) {
	oauth, _, err := s.endpoints()
	if err != nil {
		s.log.Error("login unavailable", "err", err)
		writeError(w, http.StatusServiceUnavailable, "sign-in is unavailable right now; try again shortly")
		return
	}
	ls := loginState{
		State:    randomString(),
		Nonce:    randomString(),
		Verifier: oauth2.GenerateVerifier(),
		ReturnTo: s.returnTo(r.URL.Query().Get("return_to")),
	}
	raw, _ := json.Marshal(ls) // a struct of strings always marshals
	s.setCookie(w, loginCookie, base64.RawURLEncoding.EncodeToString(raw), loginTTL)

	http.Redirect(w, r, oauth.AuthCodeURL(ls.State,
		oidc.Nonce(ls.Nonce),
		oauth2.S256ChallengeOption(ls.Verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	), http.StatusFound)
}

// Callback handles the provider's redirect. Every outcome redirects back to
// the web app; a failure adds login=failed (or cancelled, or disabled) to
// the URL, and its reason goes to the log only.
func (s *Service) Callback(w http.ResponseWriter, r *http.Request) {
	ls, ok := s.readLogin(r)
	s.clearCookie(w, loginCookie) // one attempt, one use
	if !ok {
		s.log.Warn("login callback without a login in progress")
		s.redirect(w, r, s.defaultReturn, "failed")
		return
	}

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		outcome := "failed"
		if e == "access_denied" {
			outcome = "cancelled"
		}
		s.log.Info("login not completed at the provider", "error", e)
		s.redirect(w, r, ls.ReturnTo, outcome)
		return
	}
	if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(ls.State)) != 1 || q.Get("code") == "" {
		s.log.Warn("login callback with a wrong state or no code")
		s.redirect(w, r, ls.ReturnTo, "failed")
		return
	}

	id, err := s.exchange(r.Context(), q.Get("code"), ls)
	if err != nil {
		s.log.Warn("login failed", "err", err)
		s.redirect(w, r, ls.ReturnTo, "failed")
		return
	}

	user, token, err := s.store.Login(r.Context(), id, s.ttl, s.requestID(r.Context()))
	switch {
	case errors.Is(err, ErrDisabled):
		s.log.Info("login refused: account disabled")
		s.redirect(w, r, ls.ReturnTo, "disabled")
		return
	case err != nil:
		s.log.Error("login failed: store", "err", err)
		s.redirect(w, r, ls.ReturnTo, "failed")
		return
	}
	s.setCookie(w, sessionCookie, token, s.ttl)
	s.log.Info("user signed in", "user_id", user.ID)
	http.Redirect(w, r, ls.ReturnTo, http.StatusSeeOther)
}

// exchange trades the code for tokens and verifies the ID token: signature,
// issuer, audience, expiry (by go-oidc) and the nonce (here).
func (s *Service) exchange(ctx context.Context, code string, ls loginState) (Identity, error) {
	oauth, verifier, err := s.endpoints()
	if err != nil {
		return Identity{}, err
	}
	ctx = oidc.ClientContext(ctx, s.client) // also the client oauth2 uses
	tok, err := oauth.Exchange(ctx, code, oauth2.VerifierOption(ls.Verifier))
	if err != nil {
		return Identity{}, fmt.Errorf("exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return Identity{}, errors.New("token response has no id_token")
	}
	idt, err := verifier.Verify(ctx, raw)
	if err != nil {
		return Identity{}, fmt.Errorf("verify id_token: %w", err)
	}
	if subtle.ConstantTimeCompare([]byte(idt.Nonce), []byte(ls.Nonce)) != 1 {
		return Identity{}, errors.New("id_token nonce does not match")
	}

	var claims struct {
		Email         string `json:"email"`
		EmailVerified bool   `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idt.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("read id_token claims: %w", err)
	}
	id := Identity{Issuer: idt.Issuer, Subject: idt.Subject, Name: claims.Name}
	if claims.EmailVerified {
		id.Email = claims.Email // shown to the user only; never an identifier
	}
	return id, nil
}

// Logout handles POST /auth/logout. It always clears the cookie, and answers
// 204 even without a session: the outcome the caller wanted.
func (s *Service) Logout(w http.ResponseWriter, r *http.Request) {
	s.clearCookie(w, sessionCookie)
	if c, err := r.Cookie(s.cookieName(sessionCookie)); err == nil {
		if err := s.store.Logout(r.Context(), c.Value, s.requestID(r.Context())); err != nil && !errors.Is(err, ErrNoSession) {
			s.log.Error("logout failed", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// Me handles GET /auth/me: the signed-in user, or 401.
func (s *Service) Me(w http.ResponseWriter, r *http.Request) {
	u, err := s.Authenticate(r)
	switch {
	case errors.Is(err, ErrNoSession):
		writeError(w, http.StatusUnauthorized, "not signed in")
	case err != nil:
		s.log.Error("authenticate failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	default:
		writeJSON(w, http.StatusOK, u)
	}
}

// Authenticate returns the user whose login session the request's cookie
// holds. ErrNoSession means none; any other error is the store failing.
func (s *Service) Authenticate(r *http.Request) (User, error) {
	c, err := r.Cookie(s.cookieName(sessionCookie))
	if err != nil {
		return User{}, ErrNoSession
	}
	return s.store.Lookup(r.Context(), c.Value)
}

func (s *Service) readLogin(r *http.Request) (loginState, bool) {
	c, err := r.Cookie(s.cookieName(loginCookie))
	if err != nil {
		return loginState{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(c.Value)
	if err != nil {
		return loginState{}, false
	}
	var ls loginState
	if json.Unmarshal(raw, &ls) != nil || ls.State == "" || ls.Nonce == "" || ls.Verifier == "" {
		return loginState{}, false
	}
	// Checked again: the cookie came back from the browser.
	ls.ReturnTo = s.returnTo(ls.ReturnTo)
	return ls, true
}

// returnTo accepts only an absolute URL on one of the web app's origins, so
// login can never be used to redirect a user to someone else's site.
func (s *Service) returnTo(raw string) string {
	if raw == "" || len(raw) > maxReturnTo {
		return s.defaultReturn
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return s.defaultReturn
	}
	if !s.origins[strings.ToLower(u.Scheme+"://"+u.Host)] {
		return s.defaultReturn
	}
	return u.String()
}

// redirect sends the browser back to target with login=<outcome>.
func (s *Service) redirect(w http.ResponseWriter, r *http.Request, target, outcome string) {
	u, err := url.Parse(target)
	if err != nil { // returnTo already parsed it; this cannot happen
		u, _ = url.Parse(s.defaultReturn)
	}
	q := u.Query()
	q.Set("login", outcome)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusSeeOther)
}

func randomString() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails on supported platforms
	return base64.RawURLEncoding.EncodeToString(b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
