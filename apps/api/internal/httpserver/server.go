package httpserver

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
)

// Server is the HTTP side of the application. It is an http.Handler.
type Server struct {
	handler  http.Handler
	terminal *TerminalHandler
}

// NewServer wires the routes and middleware. origins are the host patterns
// allowed to open a terminal (config.OriginHosts).
func NewServer(manager VMManager, origins []string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}

	vms := NewVMHandler(manager, log)
	terminal := NewTerminalHandler(manager, origins, log)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /vms", vms.Create)
	mux.HandleFunc("GET /vms/{id}", vms.Get)
	mux.HandleFunc("DELETE /vms/{id}", vms.Delete)
	mux.Handle("GET /ws/vms/{id}", terminal)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "ok\n")
	})

	// Outermost first: the access log must see what recovery turned a panic into.
	var h http.Handler = mux
	h = recoverPanic(log, h)
	h = accessLog(log, h)
	return &Server{handler: h, terminal: terminal}
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
