package httpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/fake"
)

const (
	timeout = 5 * time.Second
	vmID    = "vm-1"
)

// syncBuffer is a log destination that is safe to read while handlers write.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// env is the handler behind a real HTTP server, in front of the fake provider
// with one running VM.
type env struct {
	t    *testing.T
	fake *fake.Fake
	h    *TerminalHandler
	srv  *httptest.Server
	logs *syncBuffer
}

func newEnv(t *testing.T) *env {
	t.Helper()

	f := fake.New(fake.WithAutoRun())
	spec := vm.Spec{ID: vmID, Image: "/images/os.iso", MemoryMB: 128}
	if _, err := f.Create(context.Background(), spec); err != nil {
		t.Fatalf("create vm: %v", err)
	}

	logs := &syncBuffer{}
	h := NewTerminalHandler(f, []string{"localhost:3000"}, slog.New(slog.NewTextHandler(logs, nil)))
	mux := http.NewServeMux()
	mux.Handle("GET /ws/vms/{id}", h)
	srv := httptest.NewServer(mux)

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := h.Shutdown(ctx); err != nil {
			t.Errorf("cleanup shutdown: %v", err)
		}
		srv.Close()
	})
	return &env{t: t, fake: f, h: h, srv: srv, logs: logs}
}

func (e *env) url(id string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + "/ws/vms/" + id
}

// dial opens a terminal. origin may be empty, as for a non-browser client. The
// response is returned even on failure, so tests can check the status.
func (e *env) dial(id, origin string) (*websocket.Conn, *http.Response, error) {
	e.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	opts := &websocket.DialOptions{}
	if origin != "" {
		opts.HTTPHeader = http.Header{"Origin": {origin}}
	}
	c, resp, err := websocket.Dial(ctx, e.url(id), opts)
	if c != nil {
		e.t.Cleanup(func() { c.CloseNow() })
	}
	return c, resp, err
}

// mustDial dials and fails the test if the connection is refused.
func (e *env) mustDial() *websocket.Conn {
	e.t.Helper()
	c, _, err := e.dial(vmID, "")
	if err != nil {
		e.t.Fatalf("dial: %v", err)
	}
	return c
}

// guest returns the guest's side of the console once the handler has attached.
func (e *env) guest() io.ReadWriteCloser {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		g, err := e.fake.Guest(vmID)
		if err == nil {
			return g
		}
		if !errors.Is(err, fake.ErrNotAttached) || time.Now().After(deadline) {
			e.t.Fatalf("guest: %v", err)
		}
		time.Sleep(time.Millisecond)
	}
}

// consoleFree reports whether the VM's single attach slot is free, by taking
// it. It waits briefly, because the handler releases the slot as it unwinds.
func (e *env) consoleFree() bool {
	e.t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		c, err := e.fake.Console(context.Background(), vmID)
		if err == nil {
			c.Close()
			return true
		}
		if !errors.Is(err, vm.ErrAlreadyAttached) || time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func readFrame(t *testing.T, c *websocket.Conn) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	typ, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("browser read: %v", err)
	}
	if typ != websocket.MessageBinary {
		t.Fatalf("browser got a %v frame, want binary", typ)
	}
	return data
}

// readGuest reads what the browser sent. The fake's pipes block, so the read
// runs in its own goroutine and the test gives up after a timeout.
func readGuest(t *testing.T, g io.Reader) string {
	t.Helper()
	got := make(chan string, 1)
	go func() {
		buf := make([]byte, 64)
		n, _ := g.Read(buf)
		got <- string(buf[:n])
	}()
	select {
	case s := <-got:
		return s
	case <-time.After(timeout):
		t.Fatal("nothing reached the guest")
		return ""
	}
}

func statusOf(resp *http.Response) int {
	if resp == nil {
		return 0
	}
	return resp.StatusCode
}

// stubProvider fails every attach with a chosen error.
type stubProvider struct{ err error }

func (s stubProvider) Console(context.Context, string) (io.ReadWriteCloser, error) {
	return nil, s.err
}

func TestAttachErrorsBecomeStatusCodes(t *testing.T) {
	secret := "open /home/ops/images/secret.iso: permission denied"

	tests := []struct {
		name string
		err  error
		want int
	}{
		{"not found", fmt.Errorf("%w: vm-1", vm.ErrNotFound), http.StatusNotFound},
		{"not running", fmt.Errorf("%w: vm-1 is starting", vm.ErrNotRunning), http.StatusConflict},
		{"already attached", fmt.Errorf("%w: vm-1", vm.ErrAlreadyAttached), http.StatusConflict},
		{"anything else", errors.New(secret), http.StatusInternalServerError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := NewTerminalHandler(stubProvider{err: tt.err}, nil, nil)
			mux := http.NewServeMux()
			mux.Handle("GET /ws/vms/{id}", h)

			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/vms/vm-1", nil))

			if rec.Code != tt.want {
				t.Errorf("status = %d, want %d", rec.Code, tt.want)
			}
			if strings.Contains(rec.Body.String(), "secret") {
				t.Errorf("response leaks the provider's error: %q", rec.Body.String())
			}
		})
	}
}

func TestUnexpectedAttachErrorIsLoggedNotShown(t *testing.T) {
	var logs syncBuffer
	h := NewTerminalHandler(stubProvider{err: errors.New("disk on fire")}, nil,
		slog.New(slog.NewTextHandler(&logs, nil)))
	mux := http.NewServeMux()
	mux.Handle("GET /ws/vms/{id}", h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/vms/vm-1", nil))

	if strings.Contains(rec.Body.String(), "disk on fire") {
		t.Errorf("response leaks the error: %q", rec.Body.String())
	}
	if out := logs.String(); !strings.Contains(out, "disk on fire") || !strings.Contains(out, "vm-1") {
		t.Errorf("the failure was not logged with its cause and VM: %q", out)
	}
}

func TestNewHandlerWithoutLoggerDoesNotPanic(t *testing.T) {
	h := NewTerminalHandler(stubProvider{err: errors.New("boom")}, nil, nil)
	mux := http.NewServeMux()
	mux.Handle("GET /ws/vms/{id}", h)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest("GET", "/ws/vms/vm-1", nil)) // logs the 500

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
}

func TestUnknownVMIs404(t *testing.T) {
	e := newEnv(t)

	_, resp, err := e.dial("no-such-vm", "")

	if err == nil || statusOf(resp) != http.StatusNotFound {
		t.Fatalf("dial = status %d, err %v; want 404", statusOf(resp), err)
	}
}

func TestTerminalCarriesBytesBothWays(t *testing.T) {
	e := newEnv(t)
	client := e.mustDial()
	guest := e.guest()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := client.Write(ctx, websocket.MessageBinary, []byte("ls\n")); err != nil {
		t.Fatalf("browser write: %v", err)
	}
	if got := readGuest(t, guest); got != "ls\n" {
		t.Fatalf("guest got %q, want %q", got, "ls\n")
	}

	go guest.Write([]byte("kernel ready"))
	if got := readFrame(t, client); string(got) != "kernel ready" {
		t.Fatalf("browser got %q, want %q", got, "kernel ready")
	}
}

func TestSecondClientGets409AndTheFirstIsUnharmed(t *testing.T) {
	e := newEnv(t)
	first := e.mustDial()
	guest := e.guest()

	_, resp, err := e.dial(vmID, "")
	if err == nil || statusOf(resp) != http.StatusConflict {
		t.Fatalf("second dial = status %d, err %v; want 409", statusOf(resp), err)
	}

	go guest.Write([]byte("still here"))
	if got := readFrame(t, first); string(got) != "still here" {
		t.Fatalf("first client got %q after the second was refused", got)
	}
}

func TestClientLeavingFreesTheConsoleAndKeepsTheVM(t *testing.T) {
	e := newEnv(t)
	client := e.mustDial()
	e.guest() // wait until attached

	if err := client.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}

	if !e.consoleFree() {
		t.Fatal("the console stayed attached after the browser left")
	}
	if inst, err := e.fake.Get(context.Background(), vmID); err != nil || inst.Status != vm.StatusRunning {
		t.Fatalf("VM after the browser left = %+v, %v; want it still running", inst, err)
	}
	if out := e.logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a normal disconnect was logged as a problem: %s", out)
	}
}

func TestVMEndingClosesTheSocket(t *testing.T) {
	e := newEnv(t)
	client := e.mustDial()
	e.guest()

	if err := e.fake.Finish(vmID, vm.StatusStopped, "powered off"); err != nil {
		t.Fatalf("finish: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err := client.Read(ctx)
	if got := websocket.CloseStatus(err); got != websocket.StatusNormalClosure {
		t.Fatalf("browser read = %v (close status %d), want a normal closure", err, got)
	}
}

func TestOriginMustBeAllowed(t *testing.T) {
	e := newEnv(t)

	_, resp, err := e.dial(vmID, "http://evil.example")
	if err == nil || statusOf(resp) != http.StatusForbidden {
		t.Fatalf("dial from a foreign origin = status %d, err %v; want 403", statusOf(resp), err)
	}
	if !e.consoleFree() {
		t.Fatal("a rejected origin left the console attached")
	}

	if _, _, err := e.dial(vmID, "http://localhost:3000"); err != nil {
		t.Fatalf("dial from the allowed origin: %v", err)
	}
}

// This is the leak that is easy to write: the console is attached before the
// upgrade, so a failed upgrade must give it back.
func TestFailedUpgradeFreesTheConsole(t *testing.T) {
	e := newEnv(t)

	resp, err := http.Get(e.srv.URL + "/ws/vms/" + vmID) // a plain request, no upgrade headers
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode < 400 {
		t.Fatalf("status = %d, want an error status for a non-WebSocket request", resp.StatusCode)
	}

	if !e.consoleFree() {
		t.Fatal("a failed upgrade left the console attached: the VM can no longer be opened")
	}
}

func TestShutdownEndsOpenSessions(t *testing.T) {
	e := newEnv(t)
	client := e.mustDial()
	e.guest()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := e.h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v, want nil", err)
	}

	if _, _, err := client.Read(ctx); err == nil {
		t.Fatal("the browser's socket is still open after Shutdown")
	}
	if !e.consoleFree() {
		t.Fatal("Shutdown returned but the console is still attached")
	}
	if out := e.logs.String(); strings.Contains(out, "level=WARN") {
		t.Errorf("a shutdown was logged as a problem: %s", out)
	}
}

func TestNewConnectionsAreRefusedAfterShutdown(t *testing.T) {
	e := newEnv(t)
	if err := e.h.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}

	_, resp, err := e.dial(vmID, "")

	if err == nil || statusOf(resp) != http.StatusServiceUnavailable {
		t.Fatalf("dial after Shutdown = status %d, err %v; want 503", statusOf(resp), err)
	}
}

func TestShutdownIsSafeToRepeat(t *testing.T) {
	e := newEnv(t)
	for i := range 3 {
		if err := e.h.Shutdown(context.Background()); err != nil {
			t.Fatalf("Shutdown #%d = %v", i+1, err)
		}
	}
}

// Meant for -race: requests arriving while Shutdown runs must not race on the
// WaitGroup, hang, or leave a console attached.
func TestRequestsDuringShutdownDoNotRaceOrLeak(t *testing.T) {
	e := newEnv(t)

	var wg sync.WaitGroup
	for range 30 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(e.srv.URL + "/ws/vms/" + vmID)
			if err == nil {
				resp.Body.Close()
			}
		}()
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := e.h.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown = %v", err)
	}
	wg.Wait()

	if !e.consoleFree() {
		t.Fatal("a request left the console attached")
	}
}
