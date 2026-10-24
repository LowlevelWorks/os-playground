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
	vmRoutes(mux, vms, limits.TrustedProxyHops)
	// OPTIONS preflight for every /vms* path. The CORS middleware answers it;
	// without a route the mux would return 405.
	mux.HandleFunc("OPTIONS /vms", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("OPTIONS /vms/{id}", func(w http.ResponseWriter, r *http.Request) {})
	terminalRoute(mux, terminal, limits.TrustedProxyHops)

	apiMux := http.NewServeMux()
	apiMux.Handle("/api/", http.StripPrefix("/api", mux))
	apiMux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})

	// Outermost first: the access log must see what recovery turned a panic into.
	var h http.Handler = apiMux
	h = rateLimit(limits, h)
	h = cors(allowedOrigins, h)
	h = recoverPanic(log, h)
	h = accessLog(log, h)
	return &Server{handler: h, terminal: terminal}
}

// vmRoutes mounts the REST endpoints. Every one goes through withOwner, so no
// handler runs without a verified owner. Tests mount routes with this too.
func vmRoutes(mux *http.ServeMux, vms *VMHandler, proxyHops int) {
	mux.HandleFunc("POST /vms", withOwner(bearerToken, unauthorized, proxyHops, vms.Create))
	mux.HandleFunc("GET /vms/{id}", withOwner(bearerToken, vmNotFound, proxyHops, vms.Get))
	mux.HandleFunc("DELETE /vms/{id}", withOwner(bearerToken, vmNotFound, proxyHops, vms.Delete))
}

// terminalRoute mounts the console WebSocket: shutdown check first, then the
// owner, then the attach.
func terminalRoute(mux *http.ServeMux, terminal *TerminalHandler, proxyHops int) {
	mux.Handle("GET /ws/vms/{id}", terminal.admit(withOwner(wsToken, wsNotFound, proxyHops, terminal.Serve)))
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
	// The status is already sent; a failed write only means the client left.
	_, _ = w.Write(append(body, '\n'))
}

// writeError is the one error shape clients see: {"error": "..."}.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
