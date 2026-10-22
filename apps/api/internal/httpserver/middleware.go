package httpserver

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"
)

// statusRecorder remembers the status code for the access log. It must stay
// transparent: the WebSocket upgrade hijacks the connection through it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (w *statusRecorder) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusRecorder) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Hijack lets WebSocket upgrades through. A hijacked request is logged as 101.
func (w *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	conn, rw, err := http.NewResponseController(w.ResponseWriter).Hijack()
	if err == nil {
		w.status = http.StatusSwitchingProtocols
	}
	return conn, rw, err
}

// Unwrap lets http.NewResponseController reach Flush and the like.
func (w *statusRecorder) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// accessLog logs one line per request. It logs the route pattern, never the
// path or any header: the path holds a VM's ID, and the Authorization and
// Sec-WebSocket-Protocol headers hold the owner's token.
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}

		next.ServeHTTP(rec, r)

		route := r.Pattern // filled in by the mux; needs Go 1.23
		if route == "" {
			route = "unmatched"
		}
		log.Info("http request",
			"route", route,
			"status", rec.status,
			"duration", time.Since(start),
		)
	})
}

// cors emits CORS headers for the REST endpoints. The frontend is a static
// bundle served from a different origin (CloudFront in production, Next.js in
// dev), so same-origin isn't available; browsers block the response body
// unless Access-Control-Allow-Origin matches. WebSocket upgrades negotiate
// their own origin check and bypass this.
func cors(allowed []string, next http.Handler) http.Handler {
	set := make(map[string]struct{}, len(allowed))
	for _, o := range allowed {
		set[strings.ToLower(o)] = struct{}{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if origin := r.Header.Get("Origin"); origin != "" {
			if _, ok := set[strings.ToLower(origin)]; ok {
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Add("Vary", "Origin")
				if r.Method == http.MethodOptions {
					if m := r.Header.Get("Access-Control-Request-Method"); m != "" {
						h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
					}
					if hdrs := r.Header.Get("Access-Control-Request-Headers"); hdrs != "" {
						h.Set("Access-Control-Allow-Headers", hdrs)
					}
					h.Set("Access-Control-Max-Age", "600")
					w.WriteHeader(http.StatusNoContent)
					return
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// recoverPanic turns a panic in a handler into a 500 instead of a dead
// process. http.ErrAbortHandler is the standard way to abort a response, so it
// is passed on.
func recoverPanic(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				log.Error("http handler panicked", "panic", v, "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
