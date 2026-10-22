package localqemu

import (
	"bytes"
	"strings"
	"sync"
)

// tailBuffer keeps the last few KB written to it. It is QEMU's stderr: if QEMU
// refuses to start, its own complaint is the best failure reason we have.
//
// Like sink, Write never blocks and never fails.
type tailBuffer struct {
	mu        sync.Mutex
	buf       []byte
	max       int
	truncated bool // older bytes were discarded
}

// newTailBuffer returns a buffer that keeps at most max bytes.
func newTailBuffer(max int) *tailBuffer {
	if max <= 0 {
		panic("localqemu: tail size must be positive")
	}
	return &tailBuffer{max: max}
}

// Write implements io.Writer. It always reports success.
func (t *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)

	t.mu.Lock()
	defer t.mu.Unlock()

	if n >= t.max {
		t.truncated = t.truncated || n > t.max || len(t.buf) > 0
		t.buf = append(t.buf[:0], p[n-t.max:]...)
		return n, nil
	}
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.max; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
		t.truncated = true
	}
	return n, nil
}

// String returns the captured text, tidied for use in a failure reason:
// invalid UTF-8 is replaced, surrounding whitespace is removed, and if the
// start was cut off, the partial first line is dropped.
//
// The result can contain host paths and other details. Log it, but think
// before showing it to users.
func (t *tailBuffer) String() string {
	t.mu.Lock()
	b := bytes.Clone(t.buf)
	truncated := t.truncated
	t.mu.Unlock()

	s := strings.ToValidUTF8(string(b), "�")
	if truncated {
		if i := strings.IndexByte(s, '\n'); i >= 0 {
			s = s[i+1:]
		}
	}
	return strings.TrimSpace(s)
}
