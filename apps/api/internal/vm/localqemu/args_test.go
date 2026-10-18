package localqemu

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"
	"unicode"

	"github.com/os-playground-api/internal/vm"
)

func okSpec() vm.Spec {
	return vm.Spec{ID: "vm-1", Image: "/images/os.iso", MemoryMB: 128}
}

func mustArgs(t *testing.T, spec vm.Spec, opts Options) []string {
	t.Helper()
	args, err := buildArgs(spec, opts)
	if err != nil {
		t.Fatalf("buildArgs: %v", err)
	}
	return args
}

// hasPair reports whether flag is immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func count(args []string, s string) int {
	n := 0
	for _, a := range args {
		if a == s {
			n++
		}
	}
	return n
}

// Any change to the command line shows up here as a diff to review.
func TestGolden(t *testing.T) {
	want := []string{
		"-name", "vm-1",
		"-m", "128M",
		"-smp", "1",
		"-cdrom", "/images/os.iso",
		"-boot", "order=d",
		"-display", "none",
		"-monitor", "none",
		"-serial", "stdio",
		"-nic", "none",
		"-no-reboot",
		"-no-user-config",
	}
	got := mustArgs(t, okSpec(), Options{})
	if !slices.Equal(got, want) {
		t.Errorf("args changed\n got: %q\nwant: %q", got, want)
	}
}

func TestGoldenWithOptions(t *testing.T) {
	spec := okSpec()
	spec.MemoryMB = 512
	got := mustArgs(t, spec, Options{CPUs: 4, Accelerator: "kvm"})

	for _, p := range [][2]string{{"-m", "512M"}, {"-smp", "4"}, {"-accel", "kvm"}} {
		if !hasPair(got, p[0], p[1]) {
			t.Errorf("missing %s %s in %q", p[0], p[1], got)
		}
	}
	if n := count(got, "-accel"); n != 1 {
		t.Errorf("-accel appears %d times, want 1", n)
	}
	if count(mustArgs(t, okSpec(), Options{}), "-accel") != 0 {
		t.Error("-accel must be absent when no accelerator is set")
	}
}

func TestZeroOptionsAreUsable(t *testing.T) {
	if _, err := buildArgs(okSpec(), Options{}); err != nil {
		t.Fatalf("Options{} must work (defaults): %v", err)
	}
}

// The security posture, as separate tests so none is deleted by accident.
func TestSecurityInvariants(t *testing.T) {
	for _, opts := range []Options{{}, {CPUs: 8, Accelerator: "tcg"}} {
		args := mustArgs(t, okSpec(), opts)

		t.Run("no network card", func(t *testing.T) {
			if !hasPair(args, "-nic", "none") {
				t.Error("-nic none is missing")
			}
			for _, f := range []string{"-net", "-netdev"} {
				if count(args, f) != 0 {
					t.Errorf("%s must not appear", f)
				}
			}
		})
		t.Run("no monitor, no display", func(t *testing.T) {
			if !hasPair(args, "-monitor", "none") || !hasPair(args, "-display", "none") {
				t.Error("-monitor none and -display none are required")
			}
		})
		t.Run("serial is plain stdio, not multiplexed", func(t *testing.T) {
			if count(args, "-serial") != 1 || !hasPair(args, "-serial", "stdio") {
				t.Errorf("want exactly one -serial stdio, got %q", args)
			}
			for _, a := range args {
				if strings.Contains(a, "mon:") {
					t.Errorf("argument %q multiplexes the monitor", a)
				}
			}
		})
		t.Run("no forbidden flags", func(t *testing.T) {
			for _, f := range []string{
				"-nographic", "-daemonize", "-virtfs", "-fsdev", "-usb",
				"-usbdevice", "-device", "-drive", "-hda", "-hdb", "-S",
			} {
				if count(args, f) != 0 {
					t.Errorf("forbidden flag %s present", f)
				}
			}
		})
		t.Run("crash does not loop", func(t *testing.T) {
			if count(args, "-no-reboot") != 1 {
				t.Error("-no-reboot is required")
			}
		})
	}
}

func TestPathWithSpacesStaysOneArgument(t *testing.T) {
	spec := okSpec()
	spec.Image = "/Users/me/my images/os image.iso"
	args := mustArgs(t, spec, Options{})
	if !hasPair(args, "-cdrom", spec.Image) {
		t.Errorf("image was split or altered: %q", args)
	}
}

func TestDeterministic(t *testing.T) {
	a := mustArgs(t, okSpec(), Options{CPUs: 2})
	b := mustArgs(t, okSpec(), Options{CPUs: 2})
	if !slices.Equal(a, b) {
		t.Error("same input gave different output")
	}
}

func TestBadSpecsWrapErrInvalidSpec(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*vm.Spec)
	}{
		{"empty id", func(s *vm.Spec) { s.ID = "" }},
		{"uppercase id", func(s *vm.Spec) { s.ID = "VM-1" }},
		{"zero memory", func(s *vm.Spec) { s.MemoryMB = 0 }},
		{"memory above provider limit", func(s *vm.Spec) { s.MemoryMB = defaultMaxMemoryMB + 1 }},
		{"empty image", func(s *vm.Spec) { s.Image = "" }},
		{"relative image", func(s *vm.Spec) { s.Image = "build/os.iso" }},
		{"image looks like a flag", func(s *vm.Spec) { s.Image = "-nographic" }},
		{"comma", func(s *vm.Spec) { s.Image = "/a,b.iso" }},
		{"newline", func(s *vm.Spec) { s.Image = "/a\nb.iso" }},
		{"tab", func(s *vm.Spec) { s.Image = "/a\tb.iso" }},
		{"NUL", func(s *vm.Spec) { s.Image = "/a\x00b.iso" }},
		{"DEL", func(s *vm.Spec) { s.Image = "/a\x7fb.iso" }},
		{"dot-dot", func(s *vm.Spec) { s.Image = "/a/../b.iso" }},
		{"double slash", func(s *vm.Spec) { s.Image = "/a//b.iso" }},
		{"trailing slash", func(s *vm.Spec) { s.Image = "/a/b.iso/" }},
		{"dot segment", func(s *vm.Spec) { s.Image = "/a/./b.iso" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec := okSpec()
			tc.mutate(&spec)
			args, err := buildArgs(spec, Options{})
			if err == nil {
				t.Fatalf("accepted: %q", args)
			}
			if !errors.Is(err, vm.ErrInvalidSpec) {
				t.Errorf("error does not wrap ErrInvalidSpec: %v", err)
			}
		})
	}
}

// A mistake in the operator's configuration must not be reported as the
// user's fault.
func TestBadOptionsAreNotBlamedOnTheUser(t *testing.T) {
	tests := []struct {
		name string
		opts Options
	}{
		{"negative CPUs", Options{CPUs: -1}},
		{"too many CPUs", Options{CPUs: maxCPUs + 1}},
		{"unknown accelerator", Options{Accelerator: "kvm2"}},
		{"wrong case accelerator", Options{Accelerator: "KVM"}},
		{"accelerator with injected option", Options{Accelerator: "tcg,thread=multi"}},
		{"negative memory limit", Options{MaxMemoryMB: -5}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildArgs(okSpec(), tc.opts)
			if err == nil {
				t.Fatal("accepted")
			}
			if errors.Is(err, vm.ErrInvalidSpec) {
				t.Errorf("operator error wrongly wraps ErrInvalidSpec: %v", err)
			}
		})
	}
}

func TestOptionsReportEveryProblem(t *testing.T) {
	_, err := Options{CPUs: 99, Accelerator: "nope", MaxMemoryMB: -1}.normalized()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"CPUs=99", "Accelerator=\"nope\"", "MaxMemoryMB=-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s:\n%v", want, err)
		}
	}
}

func TestOptionsDefaults(t *testing.T) {
	got, err := Options{}.normalized()
	if err != nil {
		t.Fatal(err)
	}
	if got.CPUs != defaultCPUs || got.MaxMemoryMB != defaultMaxMemoryMB || got.Accelerator != "" {
		t.Errorf("defaults = %+v", got)
	}
}

func TestMemoryLimitIsInclusiveAndConfigurable(t *testing.T) {
	spec := okSpec()
	spec.MemoryMB = 256
	if _, err := buildArgs(spec, Options{MaxMemoryMB: 256}); err != nil {
		t.Errorf("memory equal to the limit should pass: %v", err)
	}
	if _, err := buildArgs(spec, Options{MaxMemoryMB: 255}); !errors.Is(err, vm.ErrInvalidSpec) {
		t.Errorf("memory above the limit = %v, want ErrInvalidSpec", err)
	}
}

// Whatever the input, an accepted result keeps the image as exactly one
// argument right after -cdrom, and no argument can smuggle in an option
// separator or a control character.
func FuzzBuildArgs(f *testing.F) {
	for _, image := range []string{
		"/images/os.iso", "/a b/c.iso", "relative.iso", "/a,b.iso", "/a\nb.iso",
		"", "/", "/a/../b", "-cdrom", "/x\x00y", "/é/ü.iso", "//a", "/a/",
	} {
		f.Add("vm-1", image, 128, 1, "")
	}
	f.Add("vm-1", "/images/os.iso", 128, 2, "kvm")
	f.Add("a", "/i", 1, 16, "tcg")

	f.Fuzz(func(t *testing.T, id, image string, mem, cpus int, accel string) {
		args, err := buildArgs(vm.Spec{ID: id, Image: image, MemoryMB: mem},
			Options{CPUs: cpus, Accelerator: accel})
		if err != nil {
			return
		}

		if args[0] != "-name" || args[1] != id {
			t.Fatalf("-name/ID wrong: %q", args)
		}
		if count(args, image) != 1 || !hasPair(args, "-cdrom", image) {
			t.Fatalf("image %q is not exactly one argument after -cdrom: %q", image, args)
		}
		if !hasPair(args, "-m", strconv.Itoa(mem)+"M") || mem < 1 || mem > defaultMaxMemoryMB {
			t.Fatalf("memory %d not accepted correctly: %q", mem, args)
		}
		for i, a := range args {
			if strings.ContainsFunc(a, unicode.IsControl) {
				t.Fatalf("argument %d has a control character: %q", i, a)
			}
			if strings.Contains(a, ",") {
				t.Fatalf("argument %d contains a comma: %q", i, a)
			}
		}
		if !hasPair(args, "-nic", "none") || !hasPair(args, "-serial", "stdio") {
			t.Fatalf("security flags missing: %q", args)
		}
		if accel != "" && !hasPair(args, "-accel", accel) {
			t.Fatalf("accelerator %q missing: %q", accel, args)
		}
	})
}
