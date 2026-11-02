package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/os-playground-api/internal/auth"
	"github.com/os-playground-api/internal/auth/authtest"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/session/storetest"
	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

// The whole server with real sign-in: auth.Service against a fake OIDC
// provider, a browser-like client with a cookie jar, and the session
// manager behind it. This is the flow the web app drives.
func TestSignInEndToEnd(t *testing.T) {
	const webOrigin = "http://localhost:3000"
	idp := authtest.NewIDP(t, "client-1")

	var handler http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)

	logins := auth.NewMemStore(time.Now)
	svc, err := auth.New(auth.Config{
		Issuer: idp.Issuer(), ClientID: "client-1", ClientSecret: "secret",
		PublicURL: srv.URL, CallbackPath: CallbackPath,
		AllowedOrigins: []string{webOrigin}, SessionTTL: time.Hour,
		Store: logins, RequestID: session.RequestID,
	})
	if err != nil {
		t.Fatal(err)
	}
	store := session.NewMemStore(time.Now)
	f := fake.New(fake.WithAutoRun())
	m, err := session.New(session.Config{
		Store: store, Provider: f, Image: "/images/os.iso", MemoryMB: 128,
		Capacity: 5, MaxPerOwner: 1, MaxPerClient: 5, IdleTimeout: time.Hour, MaxDuration: 2 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	api := NewServer(m, svc, nil, []string{webOrigin}, RateLimits{}, nil)
	t.Cleanup(func() { _ = api.Shutdown(context.Background()) })
	handler = api

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	browser := &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// do sends one request as the browser and returns the response, read.
	type response struct {
		StatusCode int
		Header     http.Header
		Body       []byte
	}
	do := func(method, path, origin string) response {
		t.Helper()
		req, _ := http.NewRequestWithContext(context.Background(), method, srv.URL+path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := browser.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response{resp.StatusCode, resp.Header, body}
	}

	if code := do("GET", "/api/v1/auth/me", webOrigin).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("me before sign-in = %d, want 401", code)
	}
	if code := do("POST", "/api/v1/playgrounds", webOrigin).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("create before sign-in = %d, want 401", code)
	}

	// The page sends the browser to log in; the API sends it to the provider.
	resp := do("GET", "/api/v1/auth/login?return_to="+url.QueryEscape(webOrigin+"/"), "")
	authorize, err := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != http.StatusFound || err != nil || !strings.HasPrefix(authorize.String(), idp.Issuer()+"/authorize") {
		t.Fatalf("login = %d to %q", resp.StatusCode, resp.Header.Get("Location"))
	}
	q := authorize.Query()

	// The user signs in at the provider, which redirects back with a code.
	idp.Grant("code-1", q.Get("code_challenge"), map[string]any{
		"sub": "google-123", "nonce": q.Get("nonce"), "email": "ada@example.com", "email_verified": true, "name": "Ada",
	})
	resp = do("GET", CallbackPath+"?"+url.Values{"state": {q.Get("state")}, "code": {"code-1"}}.Encode(), "")
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != webOrigin+"/" {
		t.Fatalf("callback = %d to %q, want 303 back to the web app", resp.StatusCode, resp.Header.Get("Location"))
	}

	resp = do("GET", "/api/v1/auth/me", webOrigin)
	var me auth.User
	if resp.StatusCode != http.StatusOK || json.Unmarshal(resp.Body, &me) != nil || me.Name != "Ada" {
		t.Fatalf("me after sign-in = %d %+v", resp.StatusCode, me)
	}

	// Signed in: the user can start a machine, from the web app only.
	if code := do("POST", "/api/v1/playgrounds", "https://evil.example").StatusCode; code != http.StatusForbidden {
		t.Fatalf("create from another site = %d, want 403", code)
	}
	resp = do("POST", "/api/v1/playgrounds", webOrigin)
	var created struct{ ID string }
	if resp.StatusCode != http.StatusCreated || json.Unmarshal(resp.Body, &created) != nil {
		t.Fatalf("create = %d", resp.StatusCode)
	}
	s, err := store.Get(context.Background(), created.ID, me.ID)
	if err != nil || s.OwnerID != me.ID {
		t.Fatalf("the session is not the signed-in user's: %+v, %v", s, err)
	}
	if code := do("GET", "/api/v1/playgrounds/"+created.ID, webOrigin).StatusCode; code != http.StatusOK {
		t.Fatalf("get = %d", code)
	}

	// The terminal authenticates with the same cookie.
	if _, err := f.Create(context.Background(), vm.Spec{ID: created.ID, Image: "/images/os.iso", MemoryMB: 128}); err != nil {
		t.Fatal(err)
	}
	storetest.SetStatus(t, store, created.ID, session.StatusRunning)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + "/api/v1/ws/playgrounds/" + created.ID
	c, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{ //nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
		HTTPClient: browser, Subprotocols: []string{wsProtocol}, HTTPHeader: http.Header{"Origin": {webOrigin}},
	})
	if err != nil {
		t.Fatalf("terminal with the login cookie: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "")

	// Sign out: the cookie stops working everywhere.
	if code := do("POST", "/api/v1/auth/logout", webOrigin).StatusCode; code != http.StatusNoContent {
		t.Fatalf("logout = %d", code)
	}
	if code := do("GET", "/api/v1/auth/me", webOrigin).StatusCode; code != http.StatusUnauthorized {
		t.Fatalf("me after sign-out = %d, want 401", code)
	}
	if code := do("GET", "/api/v1/playgrounds/"+created.ID, webOrigin).StatusCode; code != http.StatusNotFound {
		t.Fatalf("get after sign-out = %d, want 404", code)
	}

	events := logins.Events()
	if len(events) != 2 || events[0].Type != auth.EventLoggedIn || events[1].Type != auth.EventLoggedOut ||
		events[0].UserID != me.ID || events[0].RequestID == "" {
		t.Errorf("audit events = %+v, want sign-in then sign-out by %s, with request IDs", events, me.ID)
	}
}
