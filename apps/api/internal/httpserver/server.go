package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/url"
)

// Server is the HTTP side of the application. It is an http.Handler.
type Server struct {
	handler  http.Handler
	terminal *TerminalHandler
}

// NewServer wires the routes and middleware. allowedOrigins are the full
// origins (scheme://host[:port]) permitted to call the API; the WebSocket
// handler uses the host-only form for its own check, derived here.
func NewServer(manager VMManager, allowedOrigins []string, limits RateLimits, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	vms := NewVMHandler(manager, log)
	terminal := NewTerminalHandler(manager, originHosts(allowedOrigins), log)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /vms", vms.Create)
	mux.HandleFunc("GET /vms/{id}", vms.Get)
	mux.HandleFunc("DELETE /vms/{id}", vms.Delete)
	// OPTIONS preflight for every /vms* path. The CORS middleware answers it;
	// without a route the mux would return 405.
	mux.HandleFunc("OPTIONS /vms", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("OPTIONS /vms/{id}", func(w http.ResponseWriter, r *http.Request) {})
	mux.Handle("GET /ws/vms/{id}", terminal)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})

	// Outermost first: the access log must see what recovery turned a panic into.
	var h http.Handler = mux
	h = rateLimit(limits, h)
	h = cors(allowedOrigins, h)
	h = recoverPanic(log, h)
	h = accessLog(log, h)
	return &Server{handler: h, terminal: terminal}
}

// originHosts returns the host:port part of each origin, which is the form
// the nhooyr/coder WebSocket matcher expects.
func originHosts(origins []string) []string {
	hosts := make([]string, 0, len(origins))
	for _, o := range origins {
		u, err := url.Parse(o)
		if err != nil || u.Host == "" {
			continue
		}
		hosts = append(hosts, u.Host)
	}
	return hosts
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.handler.ServeHTTP(w, r) }

// Shutdown ends every open terminal session and waits for them. Call it before
// http.Server.Shutdown, which does not wait for upgraded connections.
func (s *Server) Shutdown(ctx context.Context) error { return s.terminal.Shutdown(ctx) }

// writeJSON writes v as the whole response body.
func writeJSON(w http.ResponseWriter, status int, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	w.Write(append(body, '\n'))
}

// writeError is the one error shape clients see: {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
