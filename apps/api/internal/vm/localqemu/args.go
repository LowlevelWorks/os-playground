package localqemu

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"

	"github.com/os-playground-api/internal/vm"
)

const (
	defaultCPUs        = 1
	maxCPUs            = 16
	defaultMaxMemoryMB = 4096
)

var accelerators = map[string]bool{"": true, "tcg": true, "kvm": true, "hvf": true}

type Options struct {
	CPUs int

	// Accelerator is "", "tcg", "kvm" or "hvf"; empty uses QEMU's default.
	// It must match the host: kvm needs Linux with /dev/kvm, hvf only
	// accelerates guests of the host's own architecture (so not x86_64 on
	// Apple Silicon), and tcg works anywhere but is slow. A mismatch is not
	// detected here: QEMU fails to start and the provider reports that as
	// the VM's failure reason.
	Accelerator string

	MaxMemoryMB int
}

func (o Options) normalized() (Options, error) {
	if o.CPUs == 0 {
		o.CPUs = defaultCPUs
	}
	if o.MaxMemoryMB == 0 {
		o.MaxMemoryMB = defaultMaxMemoryMB
	}

	var errs []error
	if o.CPUs < 1 || o.CPUs > maxCPUs {
		errs = append(errs, fmt.Errorf("CPUs=%d: must be between 1 and %d", o.CPUs, maxCPUs))
	}
	if o.MaxMemoryMB < 1 {
		errs = append(errs, fmt.Errorf("MaxMemoryMB=%d: must be greater than 0", o.MaxMemoryMB))
	}
	if !accelerators[o.Accelerator] {
		errs = append(errs, fmt.Errorf("Accelerator=%q: must be empty, tcg, kvm or hvf", o.Accelerator))
	}
	if err := errors.Join(errs...); err != nil {
		return Options{}, fmt.Errorf("localqemu: invalid options: %w", err)
	}
	return o, nil
}

func checkImagePath(p string) error {
	switch {
	case !filepath.IsAbs(p):
		return errors.New("must be an absolute path")
	case strings.ContainsFunc(p, unicode.IsControl):
		return errors.New("must not contain control characters")
	case strings.Contains(p, ","):
		return errors.New("must not contain a comma")
	case filepath.Clean(p) != p:
		return errors.New("must be clean: no '..', '//', '.' or trailing slash")
	}
	return nil
}

// Never add these QEMU options:
//
//	-nographic, -serial mon:stdio
//	    They multiplex the QEMU monitor onto the serial stream, which turns
//	    Ctrl-A into an escape key a user could use to stop the VM.
//	Default or user-mode networking
//	    Guests get no network. QEMU adds a NAT card unless told otherwise.
//	-virtfs, -fsdev, -usb, -device, -drive and similar
//	    They share or pass through host resources.
//	-daemonize
//	    The provider must stay the parent of the QEMU process to wait on it.

func buildArgs(spec vm.Spec, opts Options) ([]string, error) {
	opts, err := opts.normalized()
	if err != nil {
		return nil, err
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	if err := checkImagePath(spec.Image); err != nil {
		return nil, fmt.Errorf("%w: Image %q %v", vm.ErrInvalidSpec, spec.Image, err)
	}
	if spec.MemoryMB > opts.MaxMemoryMB {
		return nil, fmt.Errorf("%w: MemoryMB %d exceeds this provider's limit of %d",
			vm.ErrInvalidSpec, spec.MemoryMB, opts.MaxMemoryMB)
	}

	args := []string{
		// The ID shows up in ps output, so stray VMs can be found with pgrep -f.
		"-name", spec.ID,
		"-m", strconv.Itoa(spec.MemoryMB) + "M",
		"-smp", strconv.Itoa(opts.CPUs),

		// Boot from the image explicitly instead of relying on QEMU's default
		// boot order.
		"-cdrom", spec.Image,
		"-boot", "order=d",

		// Nothing for the host to see or poke at.
		"-display", "none",
		"-monitor", "none",

		// The console: the byte stream the provider attaches to.
		"-serial", "stdio",

		// No network card at all.
		"-nic", "none",

		// A crashing guest exits instead of rebooting in a loop.
		"-no-reboot",

		// Ignore config files in the user's home directory.
		"-no-user-config",
	}

	if opts.Accelerator != "" {
		args = append(args, "-accel", opts.Accelerator)
	}
	return args, nil
}
