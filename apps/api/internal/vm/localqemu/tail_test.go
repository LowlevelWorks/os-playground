package localqemu

import (
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

func TestTailKeepsShortInputWhole(t *testing.T) {
	b := newTailBuffer(64)
	b.Write([]byte("qemu: could not open image\n"))
	if got := b.String(); got != "qemu: could not open image" {
		t.Fatalf("got %q", got)
	}
}

func TestTailEmpty(t *testing.T) {
	if got := newTailBuffer(8).String(); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestTailKeepsTheEndAndDropsPartialFirstLine(t *testing.T) {
	b := newTailBuffer(30)
	b.Write([]byte("old line that is long enough\nfatal: disk missing\n"))
	if got := b.String(); got != "fatal: disk missing" {
		t.Fatalf("got %q", got)
	}
}

func TestTailSingleLongLineKeepsItsEnd(t *testing.T) {
	b := newTailBuffer(10)
	b.Write([]byte(strings.Repeat("a", 50) + "END"))
	if got := b.String(); !strings.HasSuffix(got, "END") || len(got) != 10 {
		t.Fatalf("got %q", got)
	}
}

func TestTailAcrossManySmallWrites(t *testing.T) {
	b := newTailBuffer(8)
	for _, w := range []string{"ab", "cd", "ef", "gh", "ij", "kl"} {
		if n, err := b.Write([]byte(w)); n != 2 || err != nil {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got := b.String(); got != "efghijkl" {
		t.Fatalf("got %q", got)
	}
}

func TestTailReplacesInvalidUTF8(t *testing.T) {
	b := newTailBuffer(16)
	b.Write([]byte("bad \xff\xfe bytes"))
	got := b.String()
	if !utf8.ValidString(got) || strings.Contains(got, "\xff") || !strings.Contains(got, "�") {
		t.Fatalf("got %q", got)
	}
}

func TestTailStringIsASnapshot(t *testing.T) {
	b := newTailBuffer(16)
	b.Write([]byte("first"))
	s := b.String()
	b.Write([]byte("second"))
	if s != "first" {
		t.Fatalf("earlier String changed to %q", s)
	}
}

func TestTailWriteNeverFails(t *testing.T) {
	b := newTailBuffer(4)
	for _, n := range []int{0, 1, 4, 5, 1000} {
		if got, err := b.Write(make([]byte, n)); got != n || err != nil {
			t.Fatalf("Write(%d) = %d, %v", n, got, err)
		}
	}
}

func TestTailConcurrent(t *testing.T) {
	b := newTailBuffer(20) // one full 15-byte line plus a partial one
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				b.Write([]byte("line of stderr\n"))
				_ = b.String()
			}
		}()
	}
	wg.Wait()
	if got := b.String(); got != "line of stderr" {
		t.Fatalf("got %q", got)
	}
}
