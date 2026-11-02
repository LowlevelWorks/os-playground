package httpserver

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestWSProtocolOffered(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		ok     bool
	}{
		{"exactly the protocol", []string{wsProtocol}, true},
		{"with spaces", []string{" " + wsProtocol + " "}, true},
		{"no header", nil, false},
		{"twice", []string{wsProtocol + ", " + wsProtocol}, false},
		{"over two headers", []string{wsProtocol, wsProtocol}, false},
		{"something else offered too", []string{wsProtocol + ", chat"}, false},
		{"a token, as the old protocol sent", []string{wsProtocol + ", token.abc"}, false},
		{"other version", []string{"os-playground.v2"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			for _, v := range tt.header {
				r.Header.Add("Sec-WebSocket-Protocol", v)
			}
			if got := wsProtocolOffered(r); got != tt.ok {
				t.Fatalf("wsProtocolOffered = %v, want %v", got, tt.ok)
			}
		})
	}
}

func TestCreateWithoutSigningInIs401(t *testing.T) {
	a := newAPI(t, 2)
	for _, token := range []string{"", "short", tokenA + "x"} {
		rec := a.doAs(token, "", "POST", "/api/v1/playgrounds")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, rec.Code)
		}
		if got := decode(t, rec)["error"]; got != "sign in to start a playground" {
			t.Errorf("token %q: error = %v", token, got)
		}
	}
	// The old bearer token no longer signs anyone in.
	r := httptest.NewRequest("POST", "/api/v1/playgrounds", nil)
	r.Header.Set("Authorization", "Bearer "+tokenA)
	rec := httptest.NewRecorder()
	a.srv.ServeHTTP(rec, r)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("bearer token: status = %d, want 401", rec.Code)
	}
}

// A store outage must never look like "signed out" or "no such VM".
func TestAuthStoreFailureIs500(t *testing.T) {
	var logs syncBuffer
	mux := http.NewServeMux()
	vmRoutes(mux, NewVMHandler(brokenVMs{}, nil), fakeAuth{err: errors.New("db down")}, slog.New(slog.NewTextHandler(&logs, nil)), 0)
	for _, method := range []string{"POST", "GET", "DELETE"} {
		path := "/api/v1/playgrounds"
		if method != "POST" {
			path += "/x"
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusInternalServerError {
			t.Errorf("%s = %d, want 500", method, rec.Code)
		}
	}
	if !strings.Contains(logs.String(), "db down") {
		t.Errorf("the failure was not logged: %s", logs.String())
	}
}

// Whatever is wrong (no VM, someone else's, no token, a bad token), the
// answer must be byte-for-byte the same, or the difference tells an attacker
// which IDs exist.
func TestAnotherOwnersVMLooksExactlyLikeNoVM(t *testing.T) {
	a := newAPI(t, 2)
	id := decode(t, a.do("POST", "/api/v1/playgrounds"))["id"].(string)

	for _, method := range []string{"GET", "DELETE"} {
		want := a.do(method, "/api/v1/playgrounds/vm-0000000000000000000000")
		if want.Code != http.StatusNotFound {
			t.Fatalf("%s of a missing VM = %d, want 404", method, want.Code)
		}
		for _, token := range []string{tokenB, "", "short"} {
			got := a.doAs(token, "", method, "/api/v1/playgrounds/"+id)
			if got.Code != want.Code || got.Body.String() != want.Body.String() ||
				!reflect.DeepEqual(withoutRequestID(got.Header()), withoutRequestID(want.Header())) {
				t.Errorf("%s as %q = %d %q %v; want what a missing VM gets: %d %q %v", method, token,
					got.Code, got.Body, got.Header(), want.Code, want.Body, want.Header())
			}
		}
	}

	if rec := a.do("GET", "/api/v1/playgrounds/"+id); rec.Code != http.StatusOK {
		t.Fatalf("the owner's GET = %d, want 200: another owner's DELETE took effect", rec.Code)
	}
}

// withoutRequestID drops X-Request-ID, which is random per request and so
// tells nothing about the session.
func withoutRequestID(h http.Header) http.Header {
	h = h.Clone()
	h.Del("X-Request-ID")
	return h
}

func TestQuotaIs409AndSaysWhoseVMItIs(t *testing.T) {
	a := newQuotaAPI(t, 4, 1)
	const here, elsewhere = "203.0.113.1:1000", "198.51.100.7:1000"
	id := decode(t, a.doAs(tokenA, here, "POST", "/api/v1/playgrounds"))["id"].(string)

	tests := []struct {
		name, token, remote string
		want                map[string]any
	}{
		{"same token", tokenA, here, map[string]any{"error": "quota", "yours": true, "id": id}},
		{"same token, other port", tokenA, "203.0.113.1:2000", map[string]any{"error": "quota", "yours": true, "id": id}},
		{"other token, same address", tokenB, here, map[string]any{"error": "quota", "yours": false}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := a.doAs(tt.token, tt.remote, "POST", "/api/v1/playgrounds")
			if rec.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409", rec.Code)
			}
			if got := decode(t, rec); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("body = %v, want exactly %v", got, tt.want)
			}
			if rec.Header().Get("Retry-After") != "" {
				t.Error("a quota refusal has Retry-After, but waiting does not help")
			}
		})
	}

	if rec := a.doAs(tokenB, elsewhere, "POST", "/api/v1/playgrounds"); rec.Code != http.StatusCreated {
		t.Fatalf("another address = %d, want 201", rec.Code)
	}
	if a.fake.Len() != 0 {
		t.Fatalf("provider holds %d VMs, want 0: POST must not call it", a.fake.Len())
	}
}

// wsAPI is the whole server listening for real, for WebSocket tests.
func wsAPI(t *testing.T, a *api) string {
	t.Helper()
	srv := httptest.NewServer(a.srv)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := a.srv.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
		srv.Close()
	})
	return "ws" + strings.TrimPrefix(srv.URL, "http")
}

// dialWS opens a terminal signed in with token (none if empty), offering
// protocols.
func dialWS(t *testing.T, url, token string, protocols []string) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	opts := &websocket.DialOptions{Subprotocols: protocols, HTTPHeader: sessionHeader(token)}
	c, resp, err := websocket.Dial(ctx, url, opts) //nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
	if err != nil {
		return nil, statusOf(resp)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, statusOf(resp)
}

func TestTerminalChecksTheOwnerBeforeUpgrading(t *testing.T) {
	a := newAPI(t, 2)
	id := decode(t, a.do("POST", "/api/v1/playgrounds"))["id"].(string)
	a.boot(id)
	base := wsAPI(t, a)

	refused := []struct {
		name      string
		token     string
		protocols []string
	}{
		{"another owner", tokenB, []string{wsProtocol}},
		{"not signed in", "", []string{wsProtocol}},
		{"no subprotocol at all", tokenA, nil},
		{"another protocol too", tokenA, []string{wsProtocol, "chat"}},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			if _, status := dialWS(t, base+"/api/v1/ws/playgrounds/"+id, tt.token, tt.protocols); status != http.StatusNotFound {
				t.Fatalf("dial = %d, want 404", status)
			}
		})
	}
	if _, status := dialWS(t, base+"/api/v1/ws/playgrounds/vm-0000000000000000000000", tokenA, []string{wsProtocol}); status != http.StatusNotFound {
		t.Fatalf("dial to a missing VM = %d, want 404", status)
	}

	c, _ := dialWS(t, base+"/api/v1/ws/playgrounds/"+id, tokenA, []string{wsProtocol})
	if c == nil {
		t.Fatal("the owner's dial was refused")
	}
	if got := c.Subprotocol(); got != wsProtocol {
		t.Fatalf("negotiated subprotocol = %q, want %q", got, wsProtocol)
	}
}

// Every path, the failures included, goes through the logger here. None of it
// may record a login token or the header that carries it.
func TestTokensNeverReachTheLogs(t *testing.T) {
	a := newQuotaAPI(t, 4, 1)
	id := decode(t, a.do("POST", "/api/v1/playgrounds"))["id"].(string)
	a.boot(id)

	a.do("POST", "/api/v1/playgrounds")                     // 409, yours
	a.doAs(tokenB, "", "POST", "/api/v1/playgrounds")       // 409, not yours
	a.doAs("short", "", "POST", "/api/v1/playgrounds")      // 401
	a.do("GET", "/api/v1/playgrounds/"+id)                  // 200
	a.doAs(tokenB, "", "GET", "/api/v1/playgrounds/"+id)    // 404
	a.doAs(tokenB, "", "DELETE", "/api/v1/playgrounds/"+id) // 404

	base := wsAPI(t, a)
	ws := []string{wsProtocol}
	dialWS(t, base+"/api/v1/ws/playgrounds/"+id, tokenB, ws) // 404
	if c, _ := dialWS(t, base+"/api/v1/ws/playgrounds/"+id, tokenA, ws); c != nil {
		dialWS(t, base+"/api/v1/ws/playgrounds/"+id, tokenA, ws) // 409, already attached
		c.Close(websocket.StatusNormalClosure, "")
	}

	if rec := a.do("DELETE", "/api/v1/playgrounds/"+id); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}

	out := a.logs.String()
	if !strings.Contains(out, "http request") {
		t.Fatalf("setup: nothing was logged: %q", out)
	}
	for _, secret := range []string{
		tokenA, tokenB, "Cookie", sessionCookieName,
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the log contains %q:\n%s", secret, out)
		}
	}
}
