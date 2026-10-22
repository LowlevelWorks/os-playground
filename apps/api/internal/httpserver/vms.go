package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/vm"
)

// vmService is what the VM endpoints need.
type vmService interface {
	Create(ctx context.Context, o session.Owner) (vm.Instance, error)
	Get(ctx context.Context, id string, o session.Owner) (vm.Instance, error)
	Delete(ctx context.Context, id string, o session.Owner) error
}

// VMManager is everything the server needs from the session manager.
type VMManager interface {
	vmService
	consoleProvider
}

var _ VMManager = (*session.Manager)(nil)

const retryAfterSeconds = 5

// VMResponse is all a client ever learns about a VM. It is deliberately not
// vm.Instance: Image is a host path and Reason can hold QEMU's own error text.
type VMResponse struct {
	ID        string    `json:"id"`
	Status    vm.Status `json:"status"`
	CreatedAt time.Time `json:"created_at"`
}

func toResponse(v vm.Instance) VMResponse {
	return VMResponse{ID: v.ID, Status: v.Status, CreatedAt: v.CreatedAt}
}

// quotaResponse is the 409 body. ID is set only when the VM is the caller's.
type quotaResponse struct {
	Error string `json:"error"`
	Yours bool   `json:"yours"`
	ID    string `json:"id,omitempty"`
}

// VMHandler serves the REST endpoints. Its methods take the caller's Owner,
// so they can only be mounted behind withOwner; see vmRoutes.
type VMHandler struct {
	svc vmService
	log *slog.Logger
}

func NewVMHandler(svc vmService, log *slog.Logger) *VMHandler {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &VMHandler{svc: svc, log: log}
}

// Create handles POST /vms. It takes no body: the server chooses everything.
func (h *VMHandler) Create(w http.ResponseWriter, r *http.Request, o session.Owner) {
	v, err := h.svc.Create(r.Context(), o)
	var quota *session.QuotaError
	switch {
	case err == nil:
	case errors.As(err, &quota):
		// No Retry-After: waiting does not help, ending a VM does.
		writeJSON(w, http.StatusConflict, quotaResponse{Error: "quota", Yours: quota.Yours, ID: quota.ID})
		return
	case errors.Is(err, vm.ErrCapacity):
		w.Header().Set("Retry-After", strconv.Itoa(retryAfterSeconds))
		writeError(w, http.StatusServiceUnavailable, "no capacity right now; try again shortly")
		return
	case r.Context().Err() != nil:
		return // the client gave up
	default:
		h.log.Error("create vm failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}

	w.Header().Set("Location", "/vms/"+v.ID)
	writeJSON(w, http.StatusCreated, toResponse(v))
}

// Get handles GET /vms/{id}. Another owner's VM is not found, like no VM.
func (h *VMHandler) Get(w http.ResponseWriter, r *http.Request, o session.Owner) {
	v, err := h.svc.Get(r.Context(), r.PathValue("id"), o)
	switch {
	case err == nil:
	case errors.Is(err, vm.ErrNotFound):
		vmNotFound(w, r)
		return
	case r.Context().Err() != nil:
		return
	default:
		h.log.Error("get vm failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, toResponse(v))
}

// Delete handles DELETE /vms/{id}. A VM that is gone or not the caller's is
// not found, as for Get.
func (h *VMHandler) Delete(w http.ResponseWriter, r *http.Request, o session.Owner) {
	err := h.svc.Delete(r.Context(), r.PathValue("id"), o)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, vm.ErrNotFound):
		vmNotFound(w, r)
	case r.Context().Err() != nil:
		// The client gave up. The VM is already hidden, and the reaper finishes
		// the deletion.
	default:
		h.log.Error("delete vm failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}
