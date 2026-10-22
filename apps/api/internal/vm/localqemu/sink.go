package localqemu

import (
	"io"
	"sync"

	"github.com/os-playground-api/internal/vm"
)

// sink receives a VM's console output (QEMU's stdout) and hands it to at most
// one reader.
//
// It is a bounded queue with one rule above all: Write never blocks and never
// fails. QEMU must keep running however slowly, or never, the browser reads. When
// the queue is full the OLDEST bytes are dropped, so a late reader sees the
// most recent output. Dropped bytes may cut an escape sequence in half; a
// terminal recovers from that on the next full repaint.
//
// The queue lives as long as the process, not as long as a reader. Output
// produced before anyone attaches (the boot banner, the first prompt) waits
// here, and a reader that detaches and comes back finds what arrived meanwhile.
// Bytes a reader has already read are gone.
//
// A sink is used from three places at once: exec's copier goroutine (Write),
// the console reader (Read, Close), and the process waiter (finish).

type sink struct {
	mu   sync.Mutex
	cond *sync.Cond // signalled on Write, finish and reader Close

	ring       []byte // fixed size: the most output we will hold
	head, size int    // oldest byte, and how many bytes are queued
	dropped    int64  // bytes discarded because the queue was full

	finished bool        // the process has ended; no more output will come
	reader   *sinkReader // the attached reader, or nil

	first chan struct{} // closed on the first byte ever written
	seen  bool
}

func newSink(max int) *sink {
	if max <= 0 {
		panic("localqemu: sink size must be positive")
	}

	s := &sink{
		ring:  make([]byte, max),
		first: make(chan struct{}),
	}
	s.cond = sync.NewCond(&s.mu)
	return s
}

// Write implements io.Writer for exec. It always reports success.
//
// Returning an error here would stop exec copying QEMU's stdout, after which
// QEMU blocks on a full pipe and the VM freezes.
func (s *sink) Write(p []byte) (int, error) {
	n := len(p)
	if n == 0 {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.finished {
		return n, nil // nobody can read it any more
	}
	if !s.seen {
		s.seen = true
		close(s.first)
	}

	max := len(s.ring)
	if n >= max {
		// Only the newest max bytes can survive, whatever was queued before.
		s.dropped += int64(s.size + n - max)
		copy(s.ring, p[n-max:])
		s.head, s.size = 0, max
	} else {
		if over := s.size + n - max; over > 0 {
			s.head = (s.head + over) % max // discard the oldest bytes
			s.size -= over
			s.dropped += int64(over)
		}
		tail := (s.head + s.size) % max
		k := copy(s.ring[tail:], p) // up to the end of the array
		copy(s.ring, p[k:])         // the rest wraps to the start
		s.size += n
	}

	s.cond.Broadcast()
	return n, nil
}

// finish marks the end of output. The process waiter calls it after
// cmd.Wait returns, which is after exec's copier has delivered every byte, so
// nothing is lost. A reader drains what is queued and then sees io.EOF.
func (s *sink) finish() {
	s.mu.Lock()
	s.finished = true
	s.cond.Broadcast()
	s.mu.Unlock()
}

// firstByte is closed when the first output byte arrives: the signal that
// the guest is alive and the VM is "running".
func (s *sink) firstByte() <-chan struct{} { return s.first }

// droppedBytes reports how much output was discarded because it was not read
// fast enough. It is for logging.
func (s *sink) droppedBytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// attach returns the reader. Only one can exist at a time; a second attach
// fails with vm.ErrAlreadyAttached until the first reader is closed.
func (s *sink) attach() (*sinkReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.reader != nil {
		return nil, vm.ErrAlreadyAttached
	}
	r := &sinkReader{s: s}
	s.reader = r
	return r, nil
}

// sinkReader is the reading side of a sink.
type sinkReader struct {
	s      *sink
	closed bool // guarded by s.mu
}

// Read blocks until output is available, the process has ended, or the
// reader is closed. After the process ends and the queue is empty it returns
// io.EOF. After Close it returns io.ErrClosedPipe.
func (r *sinkReader) Read(p []byte) (int, error) {
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()

	for {
		switch {
		case r.closed:
			return 0, io.ErrClosedPipe
		case len(p) == 0:
			return 0, nil
		case s.size > 0:
			n := min(len(p), s.size)
			first := min(n, len(s.ring)-s.head) // up to the end of the array
			copy(p, s.ring[s.head:s.head+first])
			copy(p[first:n], s.ring) // the rest wrapped to the start
			s.head = (s.head + n) % len(s.ring)
			s.size -= n
			return n, nil
		case s.finished:
			return 0, io.EOF
		}
		s.cond.Wait()
	}
}

// Close detaches the reader, wakes a Read blocked in another goroutine, and
// lets a new reader attach. It never touches the process. It is safe to call
// more than once.
func (r *sinkReader) Close() error {
	s := r.s
	s.mu.Lock()
	defer s.mu.Unlock()

	if !r.closed {
		r.closed = true
		// An open reader is always the attached one: attach refuses a new
		// reader until the old one is closed.
		s.reader = nil
		s.cond.Broadcast()
	}
	return nil
}
