package httpserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"

	"github.com/coder/websocket"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/terminal"
	"github.com/os-playground-api/internal/vm"
)

type consoleProvider interface {
	Console(ctx context.Context, id string, o session.Owner) (io.ReadWriteCloser, error)
}

// TerminalHandler serves GET /ws/vms/{id}: a WebSocket wired to a VM console.
// It is not an http.Handler: Serve needs an Owner and admit tracks sessions
// for Shutdown, and terminalRoute puts the two together.
type TerminalHandler struct {
	provider consoleProvider
	origins  []string
	log      *slog.Logger

	ctx    context.Context // parent of every session; cancelled by Shutdown
	cancel context.CancelFunc

	mu      sync.Mutex
	closing bool
	wg      sync.WaitGroup
}

func NewTerminalHandler(p consoleProvider, origins []string, log *slog.Logger) *TerminalHandler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &TerminalHandler{
		provider: p,
		origins:  origins,
		log:      log,
		ctx:      ctx,
		cancel:   cancel,
	}
}

// admit counts a request as an open session for Shutdown, or refuses it once
// Shutdown has begun. It runs before the token is read, so a request during
// shutdown is told so whatever it carries.
func (h *TerminalHandler) admit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.enter() {
			http.Error(w, "shutting down", http.StatusServiceUnavailable)
			return
		}
		defer h.wg.Done()
		next.ServeHTTP(w, r)
	})
}

// Serve attaches o to the VM's console. Ownership is checked before Accept,
// so a refusal is a plain HTTP status.
func (h *TerminalHandler) Serve(w http.ResponseWriter, r *http.Request, o session.Owner) {
	id := r.PathValue("id")
	console, err := h.provider.Console(r.Context(), id, o)
	if err != nil {
		h.attachFailed(w, r, id, err)
		return
	}

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: h.origins,
		Subprotocols:   []string{wsProtocol},
	})
	if err != nil {
		console.Close() // frees the single attach slot; Accept already replied
		return
	}

	// Bridge closes both ends before it returns.
	if err := terminal.Bridge(h.ctx, ws, console); err != nil && h.ctx.Err() == nil {
		h.log.Warn("terminal ended with error", "vm_id", id, "err", err)
	}
}

// enter registers a session, unless Shutdown has begun. Taking the lock is
// what keeps wg.Add from racing with wg.Wait.
func (h *TerminalHandler) enter() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return false
	}
	h.wg.Add(1)
	return true
}

func (h *TerminalHandler) attachFailed(w http.ResponseWriter, r *http.Request, id string, err error) {
	switch {
	case errors.Is(err, vm.ErrNotFound):
		wsNotFound(w, r)
	case errors.Is(err, vm.ErrNotRunning):
		http.Error(w, "VM is not running", http.StatusConflict)
	case errors.Is(err, vm.ErrAlreadyAttached):
		http.Error(w, "VM console is already in use", http.StatusConflict)
	case r.Context().Err() != nil:
		// The client gave up; nobody is listening for a reply.
	default:
		h.log.Error("console attach failed", "vm_id", id, "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
	}
}

// Shutdown ends every open session and waits for them, or for ctx. New
// connections are refused from the moment it is called.
func (h *TerminalHandler) Shutdown(ctx context.Context) error {
	h.mu.Lock()
	h.closing = true
	h.mu.Unlock()

	h.cancel()

	done := make(chan struct{})
	go func() {
		h.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("terminal sessions still open: %w", ctx.Err())
	}
}
