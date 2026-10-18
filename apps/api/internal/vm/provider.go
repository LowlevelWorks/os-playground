package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
)

// Status is the observed state of a VM.
//
// Allowed transitions:
//
//	provisioning -> starting -> running
//	provisioning | starting | running -> stopped | failed
//
// stopped and failed are terminal: a VM never leaves them. There is no
// "deleting" or "deleted" status; after Delete, Get returns ErrNotFound.
type Status string

const (
	StatusProvisioning Status = "provisioning" // accepted, resource being prepared
	StatusStarting     Status = "starting"     // guest is booting
	StatusRunning      Status = "running"      // guest is up; Console may attach
	StatusStopped      Status = "stopped"      // exited normally
	StatusFailed       Status = "failed"       // crashed or could not start; see Instance.Reason
	StatusUnknown      Status = "unknown"      // provider cannot tell right now
)

func (s Status) IsTerminal() bool {
	return s == StatusStopped || s == StatusFailed
}

// Spec describes the VM to create. The caller resolves user-facing choices
// (such as a profile name like "small") into these concrete numbers, so a
// provider never sees raw user input.
type Spec struct {
	// ID is chosen by the caller, typically derived from the session ID, so
	// logs, database rows and VMs correlate and retries are safe. It must be
	// a valid Kubernetes name: lowercase letters, digits and '-', starting
	// and ending with a letter or digit, at most 63 characters.
	ID string

	// Image is an opaque reference to the OS image. A local provider reads it
	// as a file path; a cluster provider reads it as a registry reference.
	Image string

	// MemoryMB is guest RAM in mebibytes. Upper limits are policy and are
	// enforced by configuration, not here.
	MemoryMB int
}

const maxIDLength = 63

var idPattern = regexp.MustCompile(
	`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`,
)

// Every provider calls this first so all of them reject the same inputs.
func (s Spec) Validate() error {
	var errors []string

	switch {
	case s.ID == "":
		errors = append(errors, "ID is required")
	case len(s.ID) > maxIDLength:
		errors = append(errors, fmt.Sprintf("ID is %d characters; the limit is %d", len(s.ID), maxIDLength))
	case !idPattern.MatchString(s.ID):
		errors = append(errors, "ID must contain only lowercase letters, digits and hyphens, and start and end with a letter or digit")
	}

	if strings.TrimSpace(s.Image) == "" {
		errors = append(errors, "Image is required")
	}

	if s.MemoryMB <= 0 {
		errors = append(errors, "MemoryMB must be greater than 0")
	}

	if len(errors) == 0 {
		return nil
	}

	return fmt.Errorf("%w: %s", ErrInvalidSpec, strings.Join(errors, ": "))
}

// Instance is a snapshot of a VM at the moment it was read.
type Instance struct {
	ID     string
	Status Status

	// Image is the reference actually used. A cluster provider reports the
	// resolved digest here so the application can record exactly what ran.
	Image string

	// Reason is a short human-readable explanation, set for failed and
	// stopped VMs and empty otherwise.
	Reason    string
	CreatedAt time.Time
}

var (
	// ErrInvalidSpec: the Spec failed validation. Fix the input; do not retry.
	ErrInvalidSpec = errors.New("vm: invalid spec")

	// ErrNotFound: no VM has this ID. Returned by Get and Console. Delete
	// never returns it.
	ErrNotFound = errors.New("vm: not found")

	// ErrAlreadyExists: Create was called with an ID that exists. Call Get to
	// see its state; this is what makes retried creates safe.
	ErrAlreadyExists = errors.New("vm: already exists")

	// ErrCapacity: no room for another VM right now. The caller should
	// report "try again later".
	ErrCapacity = errors.New("vm: capacity reached")

	// ErrNotRunning: Console was called before the VM is running, or after it
	// finished. The caller may retry while the status is still starting.
	ErrNotRunning = errors.New("vm: not running")

	// ErrAlreadyAttached: Console was called while another stream is open and
	// the provider cannot share it. A local QEMU process owns one stdio pair,
	// so it returns this; handlers must cope with it.
	ErrAlreadyAttached = errors.New("vm: console already attached")
)

// Provider creates, observes and destroys VMs.
type Provider interface {
	// Create validates spec, reserves capacity and begins creating the VM. It
	// returns quickly with the status at that moment (provisioning, or at
	// best starting); it does not wait for the guest to boot.
	//
	// Errors: ErrInvalidSpec, ErrAlreadyExists, ErrCapacity.
	Create(ctx context.Context, spec Spec) (Instance, error)

	// Get returns the VM's current state. The application observes progress
	// through Get instead of trusting Create.
	//
	// Errors: ErrNotFound.
	Get(ctx context.Context, id string) (Instance, error)

	// Delete stops the VM and releases everything it holds, including its
	// capacity slot. It is idempotent: deleting a VM that does not exist
	// returns nil. After it returns, Get reports ErrNotFound. Any console
	// still attached ends: reads return io.EOF and writes return an error.
	Delete(ctx context.Context, id string) error

	// Console attaches to the guest's serial console. Reading yields guest
	// output; writing sends input. Closing the returned stream detaches only
	// and never stops the VM.

	// When the VM stops, fails or is deleted, an attached stream ends: reads
	// return io.EOF and writes return an error. This is how callers learn the
	// VM is gone.
	//
	// Output produced while nothing is attached may be buffered or dropped; no
	// replay is guaranteed. Clients should redraw after attaching.
	//
	// Errors: ErrNotFound, ErrNotRunning, ErrAlreadyAttached.
	Console(ctx context.Context, id string) (io.ReadWriteCloser, error)
}
