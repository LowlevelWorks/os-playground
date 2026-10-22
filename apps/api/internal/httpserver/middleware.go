package httpserver

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
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
// path: until there is authentication, a VM's ID is the only thing protecting
// its console, so it must not be written to logs.
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
