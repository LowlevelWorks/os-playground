package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/coder/websocket"
)

func TestBearerToken(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		ok     bool
	}{
		{"valid", []string{"Bearer " + tokenA}, true},
		{"scheme is case-insensitive", []string{"bearer " + tokenA}, true},
		{"missing", nil, false},
		{"empty", []string{""}, false},
		{"no scheme", []string{tokenA}, false},
		{"other scheme", []string{"Basic " + tokenA}, false},
		{"too short", []string{"Bearer " + tokenA[1:]}, false},
		{"too long", []string{"Bearer " + tokenA + "A"}, false},
		{"padding", []string{"Bearer " + tokenA[1:] + "="}, false},
		{"standard base64 alphabet", []string{"Bearer " + tokenA[1:] + "+"}, false},
		{"two spaces", []string{"Bearer  " + tokenA}, false},
		{"repeated header", []string{"Bearer " + tokenA, "Bearer " + tokenB}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			for _, v := range tt.header {
				r.Header.Add("Authorization", v)
			}
			got, ok := bearerToken(r)
			if ok != tt.ok || (ok && got != tokenA) {
				t.Fatalf("bearerToken = %q, %v; want ok=%v", got, ok, tt.ok)
			}
		})
	}
}

func TestWSToken(t *testing.T) {
	tok := wsTokenPrefix + tokenA
	tests := []struct {
		name   string
		header []string
		ok     bool
	}{
		{"valid", []string{wsProtocol + ", " + tok}, true},
		{"either order", []string{tok + "," + wsProtocol}, true},
		{"over two headers", []string{wsProtocol, tok}, true},
		{"no header", nil, false},
		{"token only", []string{tok}, false},
		{"subprotocol only", []string{wsProtocol}, false},
		{"two token entries", []string{wsProtocol + ", " + tok + ", " + wsTokenPrefix + tokenB}, false},
		{"subprotocol twice", []string{wsProtocol + ", " + wsProtocol + ", " + tok}, false},
		{"something else offered too", []string{wsProtocol + ", " + tok + ", chat"}, false},
		{"malformed token", []string{wsProtocol + ", " + wsTokenPrefix + "short"}, false},
		{"other version", []string{"os-playground.v2, " + tok}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			for _, v := range tt.header {
				r.Header.Add("Sec-WebSocket-Protocol", v)
			}
			got, ok := wsToken(r)
			if ok != tt.ok || (ok && got != tokenA) {
				t.Fatalf("wsToken = %q, %v; want ok=%v", got, ok, tt.ok)
			}
		})
	}
}

func TestCreateWithoutAValidTokenIs401(t *testing.T) {
	a := newAPI(t, 2)
	for _, token := range []string{"", "short", tokenA + "x"} {
		rec := a.doAs(token, "", "POST", "/vms")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("token %q: status = %d, want 401", token, rec.Code)
		}
		if got := decode(t, rec)["error"]; got != "missing or invalid token" {
			t.Errorf("token %q: error = %v", token, got)
		}
		if rec.Header().Get("WWW-Authenticate") != "Bearer" {
			t.Errorf("token %q: no WWW-Authenticate challenge", token)
		}
	}
	if a.fake.Len() != 0 {
		t.Fatal("a VM was created without a valid token")
	}
}

// Whatever is wrong (no VM, someone else's, no token, a bad token), the
// answer must be byte-for-byte the same, or the difference tells an attacker
// which IDs exist.
func TestAnotherOwnersVMLooksExactlyLikeNoVM(t *testing.T) {
	a := newAPI(t, 2)
	id := decode(t, a.do("POST", "/vms"))["id"].(string)

	for _, method := range []string{"GET", "DELETE"} {
		want := a.do(method, "/vms/vm-0000000000000000000000")
		if want.Code != http.StatusNotFound {
			t.Fatalf("%s of a missing VM = %d, want 404", method, want.Code)
		}
		for _, token := range []string{tokenB, "", "short"} {
			got := a.doAs(token, "", method, "/vms/"+id)
			if got.Code != want.Code || got.Body.String() != want.Body.String() ||
				!reflect.DeepEqual(got.Header(), want.Header()) {
				t.Errorf("%s as %q = %d %q %v; want what a missing VM gets: %d %q %v", method, token,
					got.Code, got.Body, got.Header(), want.Code, want.Body, want.Header())
			}
		}
	}

	if a.fake.Len() != 1 {
		t.Fatal("another owner's DELETE removed the VM")
	}
	if rec := a.do("GET", "/vms/"+id); rec.Code != http.StatusOK {
		t.Fatalf("the owner's GET = %d, want 200", rec.Code)
	}
}

func TestQuotaIs409AndSaysWhoseVMItIs(t *testing.T) {
	a := newQuotaAPI(t, 4, 1)
	const here, elsewhere = "203.0.113.1:1000", "198.51.100.7:1000"
	id := decode(t, a.doAs(tokenA, here, "POST", "/vms"))["id"].(string)

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
			rec := a.doAs(tt.token, tt.remote, "POST", "/vms")
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

	if rec := a.doAs(tokenB, elsewhere, "POST", "/vms"); rec.Code != http.StatusCreated {
		t.Fatalf("another address = %d, want 201", rec.Code)
	}
	if a.fake.Len() != 2 {
		t.Fatalf("provider holds %d VMs, want 2", a.fake.Len())
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

func dialWS(t *testing.T, url string, protocols []string) (*websocket.Conn, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, url, &websocket.DialOptions{Subprotocols: protocols}) //nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
	if err != nil {
		return nil, statusOf(resp)
	}
	t.Cleanup(func() { _ = c.CloseNow() })
	return c, statusOf(resp)
}

func TestTerminalChecksTheOwnerBeforeUpgrading(t *testing.T) {
	a := newAPI(t, 2)
	id := decode(t, a.do("POST", "/vms"))["id"].(string)
	base := wsAPI(t, a)

	refused := []struct {
		name      string
		protocols []string
	}{
		{"another owner", wsOffer(tokenB)},
		{"no subprotocol at all", nil},
		{"token only", []string{wsTokenPrefix + tokenA}},
		{"subprotocol only", []string{wsProtocol}},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			if _, status := dialWS(t, base+"/ws/vms/"+id, tt.protocols); status != http.StatusNotFound {
				t.Fatalf("dial = %d, want 404", status)
			}
		})
	}
	if _, status := dialWS(t, base+"/ws/vms/vm-0000000000000000000000", wsOffer(tokenA)); status != http.StatusNotFound {
		t.Fatalf("dial to a missing VM = %d, want 404", status)
	}

	c, _ := dialWS(t, base+"/ws/vms/"+id, wsOffer(tokenA))
	if c == nil {
		t.Fatal("the owner's dial was refused")
	}
	if got := c.Subprotocol(); got != wsProtocol {
		t.Fatalf("negotiated subprotocol = %q, want %q and never the token", got, wsProtocol)
	}
}

// Every path, the failures included, goes through the logger here. None of it
// may record a token, its hash, or the headers that carry them.
func TestTokensNeverReachTheLogs(t *testing.T) {
	a := newQuotaAPI(t, 4, 1)
	id := decode(t, a.do("POST", "/vms"))["id"].(string)

	a.do("POST", "/vms")                     // 409, yours
	a.doAs(tokenB, "", "POST", "/vms")       // 409, not yours
	a.doAs("short", "", "POST", "/vms")      // 401
	a.do("GET", "/vms/"+id)                  // 200
	a.doAs(tokenB, "", "GET", "/vms/"+id)    // 404
	a.doAs(tokenB, "", "DELETE", "/vms/"+id) // 404

	base := wsAPI(t, a)
	dialWS(t, base+"/ws/vms/"+id, wsOffer(tokenB)) // 404
	if c, _ := dialWS(t, base+"/ws/vms/"+id, wsOffer(tokenA)); c != nil {
		dialWS(t, base+"/ws/vms/"+id, wsOffer(tokenA)) // 409, already attached
		c.Close(websocket.StatusNormalClosure, "")
	}

	if rec := a.do("DELETE", "/vms/"+id); rec.Code != http.StatusNoContent {
		t.Fatalf("DELETE = %d", rec.Code)
	}

	out := a.logs.String()
	if !strings.Contains(out, "http request") {
		t.Fatalf("setup: nothing was logged: %q", out)
	}
	for _, secret := range []string{
		tokenA, tokenB, hashToken(tokenA), hashToken(tokenB),
		"Bearer", "Authorization", "Sec-WebSocket-Protocol", wsTokenPrefix,
	} {
		if strings.Contains(out, secret) {
			t.Errorf("the log contains %q:\n%s", secret, out)
		}
	}
}
