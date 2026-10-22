package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

// api is the whole HTTP server over a real session manager and the fake
// provider.
type api struct {
	t    *testing.T
	fake *fake.Fake
	srv  *Server
	logs *syncBuffer
}

func newAPI(t *testing.T, capacity int) *api {
	t.Helper()
	f := fake.New(fake.WithAutoRun(), fake.WithCapacity(capacity))
	m, err := session.New(session.Config{
		Provider:    f,
		Image:       "/secret/images/os.iso",
		MemoryMB:    128,
		IdleTimeout: time.Hour,
		MaxDuration: 2 * time.Hour,
	})
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	logs := &syncBuffer{}
	srv := NewServer(m, []string{"localhost:3000"}, slog.New(slog.NewTextHandler(logs, nil)))
	return &api{t: t, fake: f, srv: srv, logs: logs}
}

func (a *api) do(method, path string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	a.srv.ServeHTTP(rec, httptest.NewRequest(method, path, nil))
	return rec
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("body is not a JSON object: %q (%v)", rec.Body.String(), err)
	}
	return m
}

func keys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func wantKeys(t *testing.T, m map[string]any) {
	t.Helper()
	if got, want := keys(m), []string{"created_at", "id", "status"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("response fields = %v, want exactly %v", got, want)
	}
}

func TestCreateReturnsTheNewVM(t *testing.T) {
	a := newAPI(t, 2)

	rec := a.do("POST", "/vms")

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q", ct)
	}
	body := decode(t, rec)
	wantKeys(t, body)
	id, _ := body["id"].(string)
	if id == "" {
		t.Fatalf("no id in the body: %v", body)
	}
	if loc := rec.Header().Get("Location"); loc != "/vms/"+id {
		t.Errorf("Location = %q, want /vms/%s", loc, id)
	}
}

func TestGetReturnsTheVMAndNothingAboutTheHost(t *testing.T) {
	a := newAPI(t, 2)
	id := decode(t, a.do("POST", "/vms"))["id"].(string)
	if err := a.fake.Finish(id, vm.StatusFailed, "open /secret/qemu/path: permission denied"); err != nil {
		t.Fatal(err)
	}

	rec := a.do("GET", "/vms/"+id)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := decode(t, rec)
	wantKeys(t, body)
	if body["id"] != id || body["status"] != "failed" {
		t.Errorf("body = %v, want id %s and status failed", body, id)
	}
	if strings.Contains(rec.Body.String(), "secret") {
		t.Errorf("response leaks host details: %s", rec.Body)
	}
}

func TestUnknownVMIs404WithAJSONError(t *testing.T) {
	a := newAPI(t, 2)

	rec := a.do("GET", "/vms/no-such-vm")

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
	if got := decode(t, rec)["error"]; got != "vm not found" {
		t.Errorf("error = %v", got)
	}
}

func TestNoCapacityIs503WithRetryAfter(t *testing.T) {
	a := newAPI(t, 2)
	a.do("POST", "/vms")
	a.do("POST", "/vms")

	rec := a.do("POST", "/vms")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Retry-After"); got != "5" {
		t.Errorf("Retry-After = %q, want 5", got)
	}
	if decode(t, rec)["error"] == nil {
		t.Error("no JSON error in the body")
	}
}

func TestDeleteIsIdempotentAndFreesTheSlot(t *testing.T) {
	a := newAPI(t, 1)
	id := decode(t, a.do("POST", "/vms"))["id"].(string)

	for i := 1; i <= 2; i++ {
		rec := a.do("DELETE", "/vms/"+id)
		if rec.Code != http.StatusNoContent || rec.Body.Len() != 0 {
			t.Fatalf("DELETE #%d = %d with body %q, want 204 and none", i, rec.Code, rec.Body)
		}
	}
	if rec := a.do("GET", "/vms/"+id); rec.Code != http.StatusNotFound {
		t.Errorf("GET after DELETE = %d, want 404", rec.Code)
	}
	if rec := a.do("POST", "/vms"); rec.Code != http.StatusCreated {
		t.Errorf("POST after DELETE = %d, want 201: the slot was not freed", rec.Code)
	}
}

func TestWrongMethodIs405(t *testing.T) {
	a := newAPI(t, 1)
	if rec := a.do("PUT", "/vms"); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("PUT /vms = %d, want 405", rec.Code)
	}
}

func TestHealthz(t *testing.T) {
	a := newAPI(t, 1)
	if rec := a.do("GET", "/healthz"); rec.Code != http.StatusOK {
		t.Fatalf("GET /healthz = %d, want 200", rec.Code)
	}
}

// brokenVMs fails every call, as a provider with a real fault would.
type brokenVMs struct{ err error }

func (b brokenVMs) Create(context.Context) (vm.Instance, error)      { return vm.Instance{}, b.err }
func (b brokenVMs) Get(context.Context, string) (vm.Instance, error) { return vm.Instance{}, b.err }
func (b brokenVMs) Delete(context.Context, string) error             { return b.err }

// An unexpected failure must never look like success, and must never reach
// the client in detail. It goes to the log.
func TestUnexpectedFailuresAre500AndLoggedNotShown(t *testing.T) {
	tests := []struct{ name, method, path string }{
		{"create", "POST", "/vms"},
		{"get", "GET", "/vms/vm-1"},
		{"delete", "DELETE", "/vms/vm-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var logs syncBuffer
			h := NewVMHandler(brokenVMs{errors.New("disk on fire")}, slog.New(slog.NewTextHandler(&logs, nil)))
			mux := http.NewServeMux()
			mux.HandleFunc("POST /vms", h.Create)
			mux.HandleFunc("GET /vms/{id}", h.Get)
			mux.HandleFunc("DELETE /vms/{id}", h.Delete)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))

			if rec.Code != http.StatusInternalServerError {
				t.Errorf("status = %d, want 500", rec.Code)
			}
			if strings.Contains(rec.Body.String(), "disk on fire") {
				t.Errorf("response leaks the error: %s", rec.Body)
			}
			if !strings.Contains(logs.String(), "disk on fire") {
				t.Errorf("the failure was not logged: %q", logs.String())
			}
		})
	}
}

func TestNewVMHandlerWithoutLoggerDoesNotPanic(t *testing.T) {
	h := NewVMHandler(brokenVMs{errors.New("boom")}, nil)
	rec := httptest.NewRecorder()
	h.Create(rec, httptest.NewRequest("POST", "/vms", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d", rec.Code)
	}
}

func TestServerShutdownReachesTheTerminalHandler(t *testing.T) {
	a := newAPI(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := a.srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}

	if rec := a.do("GET", "/ws/vms/anything"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("terminal route after Shutdown = %d, want 503", rec.Code)
	}
}
