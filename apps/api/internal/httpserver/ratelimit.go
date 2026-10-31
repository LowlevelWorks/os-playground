package httpserver

import (
	"math"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RateLimits bounds how fast one client may call the API. A client is an IPv4
// address or an IPv6 /64, since one machine usually holds a whole /64. A zero
// limit turns that limit off.
type RateLimits struct {
	// RequestsPerSecond and Burst apply to every request except /healthz. The
	// frontend polls a starting VM twice a second, so leave headroom.
	RequestsPerSecond int
	Burst             int

	// CreatesPerMinute applies to POST /playgrounds on top of the above: each one
	// boots a VM, which costs far more than any other request.
	CreatesPerMinute int

	// TrustedProxyHops is how many reverse proxies in front of the server
	// append to X-Forwarded-For. 0 uses the TCP peer address and ignores the
	// header. Anything else is only safe when the server cannot be reached
	// except through those proxies; otherwise clients pick their own address.
	TrustedProxyHops int
}

const (
	// maxTrackedClients bounds the limiter's memory (about 100 bytes each)
	// when an attacker spreads requests over many addresses.
	maxTrackedClients = 100_000
	sweepInterval     = time.Minute
)

// rateLimiter is a token bucket per client. Buckets that have refilled are
// dropped by a periodic sweep: a missing bucket and a full one behave the same.
type rateLimiter struct {
	rate  float64 // tokens per second
	burst float64
	now   func() time.Time

	mu        sync.Mutex
	buckets   map[string]*bucket
	lastSweep time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter returns nil, meaning no limit, when rate or burst is not positive.
func newRateLimiter(rate float64, burst int) *rateLimiter {
	if rate <= 0 || burst <= 0 {
		return nil
	}
	return &rateLimiter{
		rate:    rate,
		burst:   float64(burst),
		now:     time.Now,
		buckets: make(map[string]*bucket),
	}
}

// allow takes a token from key's bucket. When there is none it returns how
// long until there will be.
func (l *rateLimiter) allow(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastSweep) >= sweepInterval {
		l.sweep(now)
	}

	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxTrackedClients {
			l.sweep(now)
			if len(l.buckets) >= maxTrackedClients {
				// Every slot holds a client that is actively spending. Refusing
				// newcomers keeps memory bounded; they retry after a sweep.
				return false, sweepInterval
			}
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		return false, time.Duration((1 - b.tokens) / l.rate * float64(time.Second))
	}
	b.tokens--
	return true, 0
}

// sweep drops every bucket that has had time to refill completely.
func (l *rateLimiter) sweep(now time.Time) {
	refill := time.Duration(l.burst / l.rate * float64(time.Second))
	for k, b := range l.buckets {
		if now.Sub(b.last) >= refill {
			delete(l.buckets, k)
		}
	}
	l.lastSweep = now
}

// rateLimit answers 429 with Retry-After to a client over its limit. It sits
// inside the CORS middleware so the browser can read the 429.
func rateLimit(limits RateLimits, next http.Handler) http.Handler {
	general := newRateLimiter(float64(limits.RequestsPerSecond), limits.Burst)
	create := newRateLimiter(float64(limits.CreatesPerMinute)/60, limits.CreatesPerMinute)
	if general == nil && create == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		client := clientKey(r, limits.TrustedProxyHops)

		if ok, wait := general.allow(client); !ok {
			tooManyRequests(w, wait)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == apiPrefix+"/playgrounds" {
			if ok, wait := create.allow(client); !ok {
				tooManyRequests(w, wait)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func tooManyRequests(w http.ResponseWriter, wait time.Duration) {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	writeError(w, http.StatusTooManyRequests, "too many requests")
}

// clientKey identifies who sent r. With hops > 0 it takes the address the
// outermost trusted proxy saw, counting from the right of X-Forwarded-For,
// because every entry left of that one is whatever the client sent.
func clientKey(r *http.Request, hops int) string {
	addr := r.RemoteAddr
	if host, _, err := net.SplitHostPort(addr); err == nil {
		addr = host
	}

	if hops > 0 {
		var entries []string
		for _, v := range r.Header.Values("X-Forwarded-For") {
			entries = append(entries, strings.Split(v, ",")...)
		}
		if len(entries) >= hops {
			addr = strings.TrimSpace(entries[len(entries)-hops])
		}
	}

	ip := net.ParseIP(addr)
	if ip == nil {
		return addr
	}
	if v4 := ip.To4(); v4 != nil {
		return v4.String()
	}
	return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
}
