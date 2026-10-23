package terminal

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// These tests change the package-level ping settings, so none of them may use
// t.Parallel.

const timeout = 5 * time.Second

// testConsole stands in for a VM console. The test plays the guest: it says
// things with guestSays, powers off with guestExits, and sees what the browser
// typed on in. Close detaches, as the real console does.
type testConsole struct {
	outR *io.PipeReader // what the bridge reads
	outW *io.PipeWriter // where the "guest" writes

	in     chan []byte // what the browser typed
	closed chan struct{}
	once   sync.Once

	mu       sync.Mutex
	writeErr error
}

func newTestConsole() *testConsole {
	r, w := io.Pipe()
	return &testConsole{outR: r, outW: w, in: make(chan []byte, 16), closed: make(chan struct{})}
}

func (c *testConsole) Read(p []byte) (int, error) { return c.outR.Read(p) }

func (c *testConsole) Write(p []byte) (int, error) {
	c.mu.Lock()
	err := c.writeErr
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	select {
	case <-c.closed:
		return 0, io.ErrClosedPipe
	default:
	}
	select {
	case c.in <- bytes.Clone(p):
		return len(p), nil
	case <-c.closed:
		return 0, io.ErrClosedPipe
	}
}

func (c *testConsole) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.outR.CloseWithError(io.ErrClosedPipe) // wakes a blocked Read, like the real one
	})
	return nil
}

func (c *testConsole) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *testConsole) failWritesWith(err error) {
	c.mu.Lock()
	c.writeErr = err
	c.mu.Unlock()
}

// guestSays blocks until the bridge has read all of s.
func (c *testConsole) guestSays(t *testing.T, s string) {
	t.Helper()
	if _, err := c.outW.Write([]byte(s)); err != nil {
		t.Fatalf("guest write: %v", err)
	}
}

func (c *testConsole) guestExits() { c.outW.Close() } // the reader then sees io.EOF

// harness is a Bridge behind a real WebSocket connection.
type harness struct {
	t       *testing.T
	console *testConsole
	client  *websocket.Conn
	cancel  context.CancelFunc // cancels the context given to Bridge
	res     chan error         // Bridge's return value
	done    chan struct{}      // closed when the handler has returned
}

func start(t *testing.T) *harness {
	t.Helper()

	h := &harness{
		t:       t,
		console: newTestConsole(),
		res:     make(chan error, 1),
		done:    make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.cancel = cancel

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(h.done)
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			h.res <- err
			return
		}
		h.res <- Bridge(ctx, ws, h.console)
	}))

	dialCtx, dialCancel := context.WithTimeout(context.Background(), timeout)
	defer dialCancel()
	client, _, err := websocket.Dial(dialCtx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil) //nolint:bodyclose // websocket.Dial: "You never need to close resp.Body yourself"
	if err != nil {
		srv.Close()
		cancel()
		t.Fatalf("dial: %v", err)
	}
	h.client = client

	t.Cleanup(func() {
		cancel()
		_ = client.CloseNow()
		h.console.Close()
		select {
		case <-h.done:
		case <-time.After(timeout):
			t.Error("Bridge did not return during cleanup: something leaked")
		}
		srv.Close()
	})
	return h
}

// read returns the next binary frame the browser receives.
func (h *harness) read() []byte {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	typ, data, err := h.client.Read(ctx)
	if err != nil {
		h.t.Fatalf("browser read: %v", err)
	}
	if typ != websocket.MessageBinary {
		h.t.Fatalf("browser got a %v frame, want binary", typ)
	}
	return data
}

// readClose waits for the server to close the socket and returns how.
func (h *harness) readClose() websocket.CloseError {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_, _, err := h.client.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) {
		h.t.Fatalf("browser read: want a close, got %v", err)
	}
	return ce
}

func (h *harness) write(typ websocket.MessageType, data []byte) {
	h.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := h.client.Write(ctx, typ, data); err != nil {
		h.t.Fatalf("browser write: %v", err)
	}
}

// typed returns what reached the console's input.
func (h *harness) typed() []byte {
	h.t.Helper()
	select {
	case b := <-h.console.in:
		return b
	case <-time.After(timeout):
		h.t.Fatal("nothing reached the console")
		return nil
	}
}

// bridgeResult waits for Bridge to return and checks that it let go of the
// console on the way out. Bridge closes the console before it returns, so no
// polling is needed.
func (h *harness) bridgeResult() error {
	h.t.Helper()
	select {
	case err := <-h.res:
		if !h.console.isClosed() {
			h.t.Error("Bridge returned without closing the console")
		}
		return err
	case <-time.After(timeout):
		h.t.Fatal("Bridge did not return")
		return nil
	}
}

// fastPings makes dead-client detection quick for the duration of a test.
func fastPings(t *testing.T, interval, pingWait time.Duration) {
	t.Helper()
	oldI, oldT := pingInterval, pingTimeout
	pingInterval, pingTimeout = interval, pingWait
	t.Cleanup(func() { pingInterval, pingTimeout = oldI, oldT })
}

func TestRoundTrip(t *testing.T) {
	h := start(t)

	h.console.guestSays(t, "hello")
	if got := h.read(); string(got) != "hello" {
		t.Fatalf("browser got %q, want %q", got, "hello")
	}

	h.write(websocket.MessageBinary, []byte("ls\n"))
	if got := h.typed(); string(got) != "ls\n" {
		t.Fatalf("console got %q, want %q", got, "ls\n")
	}
}

func TestOutputBeforeTheEndIsNotLost(t *testing.T) {
	h := start(t)

	for _, s := range []string{"a", "b", "c"} {
		h.console.guestSays(t, s)
	}
	h.console.guestExits()

	var got string
	for range 3 {
		got += string(h.read())
	}
	if got != "abc" {
		t.Fatalf("browser got %q, want %q", got, "abc")
	}
	if ce := h.readClose(); ce.Code != websocket.StatusNormalClosure {
		t.Fatalf("close code = %d, want normal closure", ce.Code)
	}
}

func TestLargeOutputIsSplitIntoFrames(t *testing.T) {
	h := start(t)

	want := bytes.Repeat([]byte("0123456789"), 1000)  // 10000 bytes, over two chunks
	go func() { _, _ = h.console.outW.Write(want) }() // blocks until the bridge has read it all

	var got []byte
	for len(got) < len(want) {
		frame := h.read()
		if len(frame) > outputChunk {
			t.Fatalf("frame of %d bytes, larger than the %d chunk size", len(frame), outputChunk)
		}
		got = append(got, frame...)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("output was corrupted or reordered")
	}
}

func TestVMEndClosesTheSocketGracefully(t *testing.T) {
	h := start(t)

	h.console.guestExits()

	ce := h.readClose()
	if ce.Code != websocket.StatusNormalClosure || ce.Reason != "vm ended" {
		t.Fatalf("closed with %d %q, want normal closure \"vm ended\"", ce.Code, ce.Reason)
	}
	if err := h.bridgeResult(); err != nil {
		t.Fatalf("Bridge = %v, want nil for a VM that ended", err)
	}
}

func TestClientLeavingDetachesAndIsNotAnError(t *testing.T) {
	h := start(t)

	if err := h.client.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if err := h.bridgeResult(); err != nil {
		t.Fatalf("Bridge = %v, want nil for a client that left", err)
	}
}

func TestTextFrameIsRejected(t *testing.T) {
	h := start(t)

	h.write(websocket.MessageText, []byte("hi"))

	if ce := h.readClose(); ce.Code != websocket.StatusUnsupportedData {
		t.Fatalf("close code = %d, want unsupported data", ce.Code)
	}
	if err := h.bridgeResult(); err == nil {
		t.Fatal("Bridge = nil, want an error for a misbehaving client")
	}
}

func TestOversizedFrameIsRejected(t *testing.T) {
	h := start(t)

	h.write(websocket.MessageBinary, make([]byte, maxInputFrame+1))

	if ce := h.readClose(); ce.Code != websocket.StatusMessageTooBig {
		t.Fatalf("close code = %d, want message too big", ce.Code)
	}
	if err := h.bridgeResult(); err == nil {
		t.Fatal("Bridge = nil, want an error")
	}
	select {
	case b := <-h.console.in:
		t.Fatalf("oversized input reached the console (%d bytes)", len(b))
	default:
	}
}

func TestConsoleWriteFailureEndsTheBridge(t *testing.T) {
	h := start(t)
	stalled := errors.New("console input stalled")
	h.console.failWritesWith(stalled)

	h.write(websocket.MessageBinary, []byte("x"))

	if err := h.bridgeResult(); !errors.Is(err, stalled) {
		t.Fatalf("Bridge = %v, want %v", err, stalled)
	}
}

func TestParentCancelStopsTheBridge(t *testing.T) {
	h := start(t)

	h.cancel() // the server is shutting down

	_ = h.bridgeResult() // must return, and must have closed the console
}

// A client that vanished without a close (lid shut, Wi-Fi gone) never answers
// pings. Dropping it is what frees a console that allows one attach at a time.
// The test client never calls Read, so it never answers.
func TestUnresponsiveClientIsDropped(t *testing.T) {
	fastPings(t, 50*time.Millisecond, 100*time.Millisecond)
	h := start(t)

	err := h.bridgeResult()
	if err == nil || !strings.Contains(err.Error(), "not responding") {
		t.Fatalf("Bridge = %v, want a \"not responding\" error", err)
	}
}

// The other direction: pings must not disturb a healthy idle client. A client
// that is reading answers pings automatically.
func TestHealthyIdleClientSurvivesPings(t *testing.T) {
	fastPings(t, 30*time.Millisecond, time.Second)
	h := start(t)

	go func() {
		for {
			if _, _, err := h.client.Read(context.Background()); err != nil {
				return
			}
		}
	}()

	select {
	case err := <-h.res:
		t.Fatalf("Bridge ended on an idle but healthy client: %v", err)
	case <-time.After(400 * time.Millisecond): // a dozen ping rounds
	}
	if h.console.isClosed() {
		t.Fatal("console was closed on a healthy client")
	}

	if err := h.client.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if err := h.bridgeResult(); err != nil {
		t.Fatalf("Bridge = %v, want nil", err)
	}
}
