package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func TestABucketEmptiesAndRefills(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := newRateLimiter(2, 3) // 2 a second, burst 3
	l.now = clock.now

	for i := range 3 {
		if ok, _ := l.allow("a"); !ok {
			t.Fatalf("request %d refused within the burst", i)
		}
	}
	ok, wait := l.allow("a")
	if ok {
		t.Fatal("request past the burst allowed")
	}
	if wait != 500*time.Millisecond {
		t.Errorf("wait = %s, want 500ms", wait)
	}
	if ok, _ := l.allow("b"); !ok {
		t.Error("another client was refused because of the first")
	}

	clock.advance(500 * time.Millisecond)
	if ok, _ := l.allow("a"); !ok {
		t.Error("request refused after the bucket refilled one token")
	}
}

func TestRefilledBucketsAreSwept(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	l := newRateLimiter(1, 5)
	l.now = clock.now

	l.allow("a")
	clock.advance(sweepInterval)
	l.allow("b")

	if _, ok := l.buckets["a"]; ok {
		t.Error("an idle, refilled bucket survived the sweep")
	}
	if _, ok := l.buckets["b"]; !ok {
		t.Error("the active bucket was swept")
	}
}

func TestTooManyRequestsGets429WithRetryAfter(t *testing.T) {
	h := rateLimit(RateLimits{RequestsPerSecond: 1, Burst: 2}, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	do := func(path, remote string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", path, nil)
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	do("/vms/x", "203.0.113.1:1111")
	do("/vms/x", "203.0.113.1:2222") // another port, same client
	rec := do("/vms/x", "203.0.113.1:3333")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "1" {
		t.Errorf("Retry-After = %q, want 1", rec.Header().Get("Retry-After"))
	}
	if got := decode(t, rec)["error"]; got != "too many requests" {
		t.Errorf("error = %v", got)
	}

	if rec := do("/healthz", "203.0.113.1:4444"); rec.Code != http.StatusOK {
		t.Errorf("/healthz = %d; health checks must not be limited", rec.Code)
	}
	if rec := do("/vms/x", "198.51.100.7:1111"); rec.Code != http.StatusOK {
		t.Errorf("another client = %d, want 200", rec.Code)
	}
}

func TestCreatingVMsHasItsOwnStricterLimit(t *testing.T) {
	a := newAPI(t, 10)
	h := rateLimit(RateLimits{RequestsPerSecond: 100, Burst: 100, CreatesPerMinute: 2}, a.srv)
	do := func(method, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer "+tokenA)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	for i := range 2 {
		if rec := do("POST", "/vms"); rec.Code != http.StatusCreated {
			t.Fatalf("create %d = %d %s", i, rec.Code, rec.Body)
		}
	}
	if rec := do("POST", "/vms"); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("third create = %d, want 429", rec.Code)
	}
	if rec := do("GET", "/vms/nope"); rec.Code != http.StatusNotFound {
		t.Errorf("other requests were refused too: %d", rec.Code)
	}
}

func TestClientKey(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		xff    []string
		hops   int
		want   string
	}{
		{"peer address", "203.0.113.1:5000", nil, 0, "203.0.113.1"},
		{"header ignored without trusted proxies", "203.0.113.1:5000", []string{"1.1.1.1"}, 0, "203.0.113.1"},
		{"one proxy", "10.0.0.2:5000", []string{"6.6.6.6, 198.51.100.9"}, 1, "198.51.100.9"},
		{"two proxies over two headers", "10.0.0.2:5000", []string{"6.6.6.6, 198.51.100.9", "10.0.0.1"}, 2, "198.51.100.9"},
		{"header shorter than hops", "203.0.113.1:5000", nil, 1, "203.0.113.1"},
		{"ipv6 grouped by /64", "[2001:db8:1:2:aaaa::1]:5000", nil, 0, "2001:db8:1:2::/64"},
		{"ipv4-mapped ipv6", "[::ffff:203.0.113.1]:5000", nil, 0, "203.0.113.1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			r.RemoteAddr = tt.remote
			for _, v := range tt.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if got := clientKey(r, tt.hops); got != tt.want {
				t.Errorf("clientKey = %q, want %q", got, tt.want)
			}
		})
	}
}
