package localqemu

import (
	"bytes"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
)

const wait = 2 * time.Second

type readResult struct {
	n   int
	err error
}

// readAsync starts a Read that may block and returns its result channel.
func readAsync(r io.Reader, size int) (<-chan readResult, []byte) {
	buf := make([]byte, size)
	ch := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		ch <- readResult{n, err}
	}()
	return ch, buf
}

func mustAttach(t *testing.T, s *sink) *sinkReader {
	t.Helper()
	r, err := s.attach()
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	return r
}

func readAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	done := make(chan struct{})
	var out []byte
	var err error
	go func() { out, err = io.ReadAll(r); close(done) }()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("ReadAll did not finish")
	}
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return out
}

func TestSinkWriteThenRead(t *testing.T) {
	s := newSink(64)
	s.Write([]byte("hello "))
	s.Write([]byte("world"))
	r := mustAttach(t, s)

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "hello world" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
}

func TestSinkBacklogWaitsForFirstReader(t *testing.T) {
	// The boot banner is written before anyone attaches.
	s := newSink(64)
	s.Write([]byte("boot banner\n"))
	s.finish()
	if got := readAll(t, mustAttach(t, s)); string(got) != "boot banner\n" {
		t.Fatalf("got %q", got)
	}
}

func TestSinkReadBlocksUntilWrite(t *testing.T) {
	s := newSink(64)
	r := mustAttach(t, s)
	ch, buf := readAsync(r, 16)

	select {
	case <-ch:
		t.Fatal("Read returned with nothing to read")
	case <-time.After(50 * time.Millisecond):
	}

	s.Write([]byte("hi"))
	select {
	case res := <-ch:
		if res.err != nil || string(buf[:res.n]) != "hi" {
			t.Fatalf("Read = %q, %v", buf[:res.n], res.err)
		}
	case <-time.After(wait):
		t.Fatal("Read did not wake on Write")
	}
}

func TestSinkPartialReadsKeepTheRest(t *testing.T) {
	s := newSink(64)
	s.Write([]byte("abcdef"))
	r := mustAttach(t, s)

	var got []byte
	buf := make([]byte, 2)
	for i := 0; i < 3; i++ {
		n, err := r.Read(buf)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "abcdef" {
		t.Fatalf("got %q", got)
	}
}

func TestSinkEmptyReadBufferReturnsAtOnce(t *testing.T) {
	r := mustAttach(t, newSink(8))
	if n, err := r.Read(nil); n != 0 || err != nil {
		t.Fatalf("Read(nil) = %d, %v", n, err)
	}
}

// The property that matters most: no reader means no blocking.
func TestSinkWriteNeverBlocksWithoutReader(t *testing.T) {
	s := newSink(1024)
	chunk := bytes.Repeat([]byte("x"), 32<<10)

	done := make(chan struct{})
	go func() {
		for i := 0; i < 320; i++ { // ~10 MB into a 1 KB queue
			if n, err := s.Write(chunk); n != len(chunk) || err != nil {
				t.Errorf("Write = %d, %v", n, err)
			}
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(wait):
		t.Fatal("Write blocked with no reader")
	}
	if s.size > 1024 {
		t.Fatalf("queue grew to %d bytes", s.size)
	}
	if want := int64(320*len(chunk) - 1024); s.droppedBytes() != want {
		t.Errorf("dropped = %d, want %d", s.droppedBytes(), want)
	}
}

func TestSinkKeepsNewestBytesWhenFull(t *testing.T) {
	s := newSink(8)
	s.Write([]byte("0123456789")) // larger than the whole queue
	s.Write([]byte("AB"))
	s.finish()
	// The 8 newest bytes of "0123456789AB".
	if got := readAll(t, mustAttach(t, s)); string(got) != "456789AB" {
		t.Fatalf("got %q, want %q", got, "456789AB")
	}
}

func TestSinkWriteAfterFinishIsDiscardedWithoutError(t *testing.T) {
	s := newSink(8)
	s.finish()
	if n, err := s.Write([]byte("late")); n != 4 || err != nil {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if got := readAll(t, mustAttach(t, s)); len(got) != 0 {
		t.Fatalf("got %q after finish", got)
	}
}

func TestSinkFinishDeliversQueuedDataThenEOF(t *testing.T) {
	s := newSink(64)
	s.Write([]byte("last words"))
	s.finish()
	r := mustAttach(t, s)

	buf := make([]byte, 64)
	n, err := r.Read(buf)
	if err != nil || string(buf[:n]) != "last words" {
		t.Fatalf("first Read = %q, %v", buf[:n], err)
	}
	if n, err := r.Read(buf); n != 0 || err != io.EOF {
		t.Fatalf("second Read = %d, %v; want 0, EOF", n, err)
	}
}

func TestSinkFinishWakesBlockedReader(t *testing.T) {
	s := newSink(64)
	ch, _ := readAsync(mustAttach(t, s), 16)
	time.Sleep(20 * time.Millisecond)
	s.finish()
	select {
	case res := <-ch:
		if res.err != io.EOF {
			t.Fatalf("err = %v, want EOF", res.err)
		}
	case <-time.After(wait):
		t.Fatal("blocked Read not woken by finish")
	}
}

func TestSinkCloseWakesBlockedReader(t *testing.T) {
	s := newSink(64)
	r := mustAttach(t, s)
	ch, _ := readAsync(r, 16)
	time.Sleep(20 * time.Millisecond)
	r.Close()
	select {
	case res := <-ch:
		if !errors.Is(res.err, io.ErrClosedPipe) {
			t.Fatalf("err = %v, want ErrClosedPipe", res.err)
		}
	case <-time.After(wait):
		t.Fatal("blocked Read not woken by Close")
	}
}

func TestSinkReadAfterCloseFails(t *testing.T) {
	s := newSink(8)
	s.Write([]byte("data"))
	r := mustAttach(t, s)
	r.Close()
	if _, err := r.Read(make([]byte, 4)); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err = %v, want ErrClosedPipe", err)
	}
}

func TestSinkOneReaderAtATime(t *testing.T) {
	s := newSink(8)
	r := mustAttach(t, s)
	if _, err := s.attach(); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Fatalf("second attach = %v, want ErrAlreadyAttached", err)
	}
	r.Close()
	r.Close() // twice is fine
	if _, err := s.attach(); err != nil {
		t.Fatalf("attach after Close: %v", err)
	}
}

// A stale reader's Close must not detach the reader that replaced it.
func TestSinkStaleCloseDoesNotDetachNewReader(t *testing.T) {
	s := newSink(8)
	old := mustAttach(t, s)
	old.Close()
	_ = mustAttach(t, s)
	old.Close()
	if _, err := s.attach(); !errors.Is(err, vm.ErrAlreadyAttached) {
		t.Fatalf("new reader was detached by the old reader's Close: %v", err)
	}
}

func TestSinkReattachFindsOutputProducedWhileAway(t *testing.T) {
	s := newSink(64)
	r := mustAttach(t, s)
	s.Write([]byte("one"))
	buf := make([]byte, 8)
	n, _ := r.Read(buf)
	if string(buf[:n]) != "one" {
		t.Fatalf("got %q", buf[:n])
	}
	r.Close()

	s.Write([]byte("two"))
	s.Write([]byte("three"))
	s.finish()
	if got := readAll(t, mustAttach(t, s)); string(got) != "twothree" {
		t.Fatalf("got %q", got)
	}
}

func TestSinkFirstByte(t *testing.T) {
	s := newSink(8)
	closed := func() bool {
		select {
		case <-s.firstByte():
			return true
		default:
			return false
		}
	}
	if closed() {
		t.Fatal("closed before any write")
	}
	s.Write(nil)
	s.Write([]byte{})
	if closed() {
		t.Fatal("an empty write counted as output")
	}
	s.Write([]byte("x"))
	if !closed() {
		t.Fatal("not closed after first byte")
	}
	s.Write([]byte("y")) // must not close twice (would panic)
}

func TestSinkFirstByteSurvivesDrop(t *testing.T) {
	s := newSink(2)
	s.Write([]byte("abcdef"))
	select {
	case <-s.firstByte():
	default:
		t.Fatal("firstByte not closed")
	}
}

func TestNewSinkRejectsBadSize(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("newSink(0) did not panic")
		}
	}()
	newSink(0)
}

// Producer, consumer and finish all at once under -race. Every byte written
// must be read or counted as dropped, exactly once.
func TestSinkConcurrentConservation(t *testing.T) {
	const writes, chunk = 2000, 100
	s := newSink(512)
	r := mustAttach(t, s)

	var wg sync.WaitGroup
	var read int64
	wg.Add(1)
	go func() {
		defer wg.Done()
		buf := make([]byte, 37)
		for {
			n, err := r.Read(buf)
			read += int64(n)
			if err != nil {
				return
			}
		}
	}()

	p := bytes.Repeat([]byte("z"), chunk)
	for i := 0; i < writes; i++ {
		s.Write(p)
	}
	s.finish()
	wg.Wait()

	if total := read + s.droppedBytes(); total != writes*chunk {
		t.Fatalf("read %d + dropped %d = %d, want %d", read, s.droppedBytes(), total, writes*chunk)
	}
}

// Random writes and reads against a model that is obviously correct: a
// byte slice from which only the newest max bytes are kept.
func FuzzSinkAgainstModel(f *testing.F) {
	f.Add([]byte{4, 0, 9, 1, 3, 0, 20, 1, 5})
	f.Add([]byte{1, 0, 3, 0, 3, 1, 1})
	f.Add([]byte{16, 0, 40, 1, 7, 1, 7, 0, 2, 1, 50})

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 1 {
			return
		}
		max := 1 + int(data[0])%16
		data = data[1:]

		s := newSink(max)
		r, _ := s.attach()
		var model []byte
		var written, readTotal int64
		counter := byte(0)

		for len(data) >= 2 {
			op, arg := data[0], int(data[1])
			data = data[2:]

			if op%2 == 0 { // write arg%(2*max+1) bytes
				n := arg % (2*max + 1)
				p := make([]byte, n)
				for i := range p {
					counter++
					p[i] = counter
				}
				s.Write(p)
				model = append(model, p...)
				if len(model) > max {
					model = model[len(model)-max:]
				}
				written += int64(n)
			} else { // read, only when the model says it will not block
				if len(model) == 0 {
					continue
				}
				size := 1 + arg%(max+3)
				buf := make([]byte, size)
				n, err := r.Read(buf)
				if err != nil {
					t.Fatalf("Read: %v", err)
				}
				want := model[:min(size, len(model))]
				if !bytes.Equal(buf[:n], want) {
					t.Fatalf("read %v, model says %v", buf[:n], want)
				}
				model = model[n:]
				readTotal += int64(n)
			}
		}

		s.finish()
		rest, err := io.ReadAll(r)
		if err != nil || !bytes.Equal(rest, model) {
			t.Fatalf("drain = %v, %v; model says %v", rest, err, model)
		}
		readTotal += int64(len(rest))
		if readTotal+s.droppedBytes() != written {
			t.Fatalf("read %d + dropped %d != written %d", readTotal, s.droppedBytes(), written)
		}
	})
}
