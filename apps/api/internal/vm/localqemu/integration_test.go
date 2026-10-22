//go:build integration

package localqemu

import (
	"context"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/os-playground-api/internal/vm"
	"github.com/os-playground-api/internal/vm/vmtest"
)

// These tests run the real QEMU against your real OS image. They are excluded
// from a normal `go test`; run them with:
//
//	PLAYGROUND_OS_IMAGE=/absolute/path/to/os.iso \
//	  go test -tags integration -race -count=1 -v -run TestRealQEMU ./internal/vm/localqemu/
//
// Optional: PLAYGROUND_QEMU_BIN (default qemu-system-x86_64) and
// PLAYGROUND_QEMU_ACCEL (tcg, kvm or hvf; default is QEMU's own choice).
func realProvider(t *testing.T, maxVMs int) (*Provider, vm.Spec) {
	t.Helper()
	image := os.Getenv("PLAYGROUND_OS_IMAGE")
	if image == "" {
		t.Skip("set PLAYGROUND_OS_IMAGE to the absolute path of your OS ISO")
	}
	p, err := New(Config{
		QEMUBin:      os.Getenv("PLAYGROUND_QEMU_BIN"),
		Options:      Options{Accelerator: os.Getenv("PLAYGROUND_QEMU_ACCEL")},
		MaxVMs:       maxVMs,
		StartTimeout: 60 * time.Second,
		Logger:       slog.New(slog.NewTextHandler(os.Stderr, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err) // the image is set, so QEMU is expected to exist
	}
	t.Cleanup(func() {
		ctx, cancel := withTimeout(20 * time.Second)
		defer cancel()
		if err := p.Close(ctx); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return p, vm.Spec{ID: "real-1", Image: image, MemoryMB: 128}
}

// Your kernel really boots under the flags buildArgs produces, and its first
// serial output reaches a console.
func TestRealQEMUBoots(t *testing.T) {
	p, spec := realProvider(t, 1)
	ctx, cancel := withTimeout(2 * time.Minute)
	defer cancel()

	if _, err := p.Create(ctx, spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	deadline := time.Now().Add(60 * time.Second)
	for {
		got, err := p.Get(ctx, spec.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == vm.StatusRunning {
			break
		}
		if got.Status.IsTerminal() || time.Now().After(deadline) {
			t.Fatalf("never ran: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}

	c, err := p.Console(ctx, spec.ID)
	if err != nil {
		t.Fatalf("Console: %v", err)
	}
	defer c.Close()

	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("no serial output from the guest: %d bytes, %v", n, err)
	}
	t.Logf("first serial output (%d bytes): %q", n, buf[:n])

	// The first bytes may come from the bootloader, not your kernel. Name text
	// only the kernel prints (a banner or the shell prompt) to prove it booted.
	if marker := os.Getenv("PLAYGROUND_BOOT_MARKER"); marker != "" {
		if !readUntil(c, string(buf[:n]), marker, 60*time.Second) {
			t.Fatalf("the kernel never printed %q within 60s: the VM may be stuck in the bootloader", marker)
		}
		t.Logf("saw %q: the kernel booted", marker)
	} else {
		t.Log("PLAYGROUND_BOOT_MARKER is not set: only the first serial bytes were checked, " +
			"and they may be the bootloader's, not your kernel's")
	}

	// Control characters must not reach a QEMU monitor: Ctrl-A x would quit
	// QEMU if the serial port were multiplexed with it. The VM must survive.
	if _, err := c.Write([]byte{0x01, 'x'}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if got, _ := p.Get(ctx, spec.ID); got.Status != vm.StatusRunning {
		t.Fatalf("Ctrl-A x changed the VM to %s: the serial port reaches a QEMU monitor", got.Status)
	}
}

// The same behavioural contract the fake and the fake-QEMU provider pass, now
// against the real thing.
func TestRealQEMUContract(t *testing.T) {
	_, spec := realProvider(t, 2)
	vmtest.Run(t, vmtest.Factory{
		New: func(t *testing.T) vm.Provider {
			p, _ := realProvider(t, 2)
			return p
		},
		Spec:       func(id string) vm.Spec { s := spec; s.ID = id; return s },
		Capacity:   2,
		RunTimeout: 90 * time.Second,
	})
}

func withTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// readUntil reads from r until the accumulated output contains marker. seen is
// what was already read. It reports whether the marker appeared in time.
func readUntil(r interface{ Read([]byte) (int, error) }, seen, marker string, d time.Duration) bool {
	found := make(chan bool, 1)
	go func() {
		acc, buf := seen, make([]byte, 4096)
		for !strings.Contains(acc, marker) {
			n, err := r.Read(buf)
			acc += string(buf[:n])
			if err != nil {
				found <- strings.Contains(acc, marker)
				return
			}
		}
		found <- true
	}()
	select {
	case ok := <-found:
		return ok
	case <-time.After(d):
		return false
	}
}
