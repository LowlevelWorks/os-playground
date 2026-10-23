package httpserver

import (
	"bufio"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAccessLogRecordsTheRouteNotTheVMID(t *testing.T) {
	a := newAPI(t, 1)

	a.do("GET", "/vms/vm-topsecret0123456789")

	out := a.logs.String()
	if strings.Contains(out, "topsecret") {
		t.Fatalf("the access log contains a VM ID: %s", out)
	}
	if !strings.Contains(out, "GET /vms/{id}") || !strings.Contains(out, "status=404") {
		t.Fatalf("the access log lacks the route or status: %s", out)
	}
}

func TestAPanicBecomesA500AndIsLoggedWithItsStatus(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := accessLog(log, recoverPanic(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("kaboom")
	})))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "kaboom") {
		t.Errorf("the panic value reached the client: %s", rec.Body)
	}
	out := logs.String()
	for _, want := range []string{"kaboom", "stack=", "status=500"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q: %s", want, out)
		}
	}

	// The process, and the handler, keep working.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("second request = %d", rec.Code)
	}
}

func TestAbortHandlerIsNotSwallowed(t *testing.T) {
	h := recoverPanic(slog.New(slog.NewTextHandler(&syncBuffer{}, nil)),
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { panic(http.ErrAbortHandler) }))

	defer func() {
		//nolint:errorlint // recover() value, not an error chain; same check as net/http
		if got := recover(); got != http.ErrAbortHandler {
			t.Fatalf("recovered %v, want http.ErrAbortHandler passed through", got)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
}

// The WebSocket upgrade hijacks the connection through the middleware. This is
// the same step, without the WebSocket library.
func TestAConnectionCanBeHijackedThroughTheMiddleware(t *testing.T) {
	var logs syncBuffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	srv := httptest.NewServer(accessLog(log, recoverPanic(log, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := http.NewResponseController(w).Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusNotImplemented)
			return
		}
		defer conn.Close()
		_, _ = rw.WriteString("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: test\r\n\r\n")
		rw.Flush()
	}))))
	defer srv.Close()

	var d net.Dialer
	conn, err := d.DialContext(t.Context(), "tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("GET / HTTP/1.1\r\nHost: x\r\n\r\n")); err != nil {
		t.Fatal(err)
	}

	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || !strings.HasPrefix(line, "HTTP/1.1 101") {
		t.Fatalf("first response line = %q (%v), want 101: the middleware blocks hijacking", line, err)
	}

	deadline := time.Now().Add(timeout)
	for !strings.Contains(logs.String(), "status=101") {
		if time.Now().After(deadline) {
			t.Fatalf("a hijacked request was not logged as 101: %q", logs.String())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTheFirstStatusWins(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &statusRecorder{ResponseWriter: inner}

	rec.WriteHeader(http.StatusNotFound)
	rec.WriteHeader(http.StatusInternalServerError)

	if rec.status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, the first one written", rec.status)
	}
}
