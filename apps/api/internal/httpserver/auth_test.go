package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Writes from another site are refused before they reach a handler, even if
// the browser sent the login cookie (SameSite=Lax is the first line; this is
// the second, and it also covers sibling subdomains).
func TestCrossOriginWritesAreRefused(t *testing.T) {
	a := newAPI(t, 2)
	for _, tc := range []struct {
		method, path, origin string
		want                 int
	}{
		{"POST", "/api/v1/playgrounds", "https://evil.example", http.StatusForbidden},
		{"POST", "/api/v1/playgrounds", "http://sub.localhost:3000", http.StatusForbidden},
		{"POST", "/api/v1/auth/logout", "https://evil.example", http.StatusForbidden},
		{"DELETE", "/api/v1/playgrounds/x", "https://evil.example", http.StatusForbidden},
		{"POST", "/api/v1/playgrounds", "http://localhost:3000", http.StatusCreated},
		{"POST", "/api/v1/playgrounds", "HTTP://LOCALHOST:3000", http.StatusCreated}, // origins compare case-insensitively
		{"GET", "/api/v1/auth/me", "https://evil.example", http.StatusOK},            // reads are CORS's job
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Header.Set("Origin", tc.origin)
		withSession(r, tokenA)
		rec := httptest.NewRecorder()
		a.srv.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s %s from %s = %d, want %d", tc.method, tc.path, tc.origin, rec.Code, tc.want)
		}
	}
	if a.fake.Len() != 0 {
		t.Error("a refused write reached the provider")
	}
}

// The web app calls with credentials, which CORS allows only with an exact
// origin and Allow-Credentials.
func TestCORSAllowsCredentialsForTheWebAppOnly(t *testing.T) {
	a := newAPI(t, 2)
	for origin, want := range map[string]string{"http://localhost:3000": "true", "https://evil.example": ""} {
		r := httptest.NewRequest("OPTIONS", "/api/v1/auth/me", nil)
		r.Header.Set("Origin", origin)
		r.Header.Set("Access-Control-Request-Method", "GET")
		rec := httptest.NewRecorder()
		a.srv.ServeHTTP(rec, r)
		if got := rec.Header().Get("Access-Control-Allow-Credentials"); got != want {
			t.Errorf("from %s: Allow-Credentials = %q, want %q", origin, got, want)
		}
		if want != "" && rec.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Errorf("from %s: Allow-Origin = %q, want the exact origin", origin, rec.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestAuthRoutesAreMounted(t *testing.T) {
	a := newAPI(t, 2)
	for _, tc := range []struct {
		method, path, token string
		want                int
	}{
		{"GET", "/api/v1/auth/login", "", http.StatusFound},
		{"GET", "/api/v1/auth/callback", "", http.StatusSeeOther},
		{"GET", "/api/v1/auth/me", "", http.StatusUnauthorized},
		{"GET", "/api/v1/auth/me", tokenA, http.StatusOK},
		{"POST", "/api/v1/auth/logout", tokenA, http.StatusNoContent},
		{"POST", "/api/v1/auth/me", tokenA, http.StatusMethodNotAllowed},
	} {
		if rec := a.doAs(tc.token, "", tc.method, tc.path); rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}

// Without sign-in configured the server still runs, says why login fails,
// and lets nobody start a machine.
func TestServerWithoutSignIn(t *testing.T) {
	srv := NewServer(struct {
		brokenVMs
		stubProvider
	}{}, nil, nil, []string{"http://localhost:3000"}, RateLimits{}, nil)
	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", "/api/v1/auth/login", http.StatusServiceUnavailable},
		{"GET", "/api/v1/auth/me", http.StatusUnauthorized},
		{"POST", "/api/v1/auth/logout", http.StatusNoContent},
		{"POST", "/api/v1/playgrounds", http.StatusUnauthorized},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		withSession(r, tokenA)
		rec := httptest.NewRecorder()
		srv.ServeHTTP(rec, r)
		if rec.Code != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, rec.Code, tc.want)
		}
	}
}
