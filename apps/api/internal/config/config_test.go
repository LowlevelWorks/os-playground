package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("ListenAddr = %q, want loopback only", cfg.ListenAddr)
	}
	if cfg.VMMemoryMB != 128 || cfg.MaxSessions != 2 || cfg.MaxVMsPerClient != 1 {
		t.Errorf("memory/sessions/per client = %d/%d/%d, want 128/2/1",
			cfg.VMMemoryMB, cfg.MaxSessions, cfg.MaxVMsPerClient)
	}
	if cfg.IdleTimeout != 15*time.Minute || cfg.MaxDuration != time.Hour {
		t.Errorf("timeouts = %s/%s", cfg.IdleTimeout, cfg.MaxDuration)
	}
	if cfg.RateLimitRPS == 0 || cfg.CreateLimitPerMinute == 0 || cfg.TrustedProxyHops != 0 {
		t.Errorf("rate limits = %d rps, %d creates/min, %d hops; want limits on and no trusted proxy",
			cfg.RateLimitRPS, cfg.CreateLimitPerMinute, cfg.TrustedProxyHops)
	}
}

// The per-client limit may exceed the global one: it then never bites, which
// is a valid choice for a single-user deployment.
func TestPerClientLimitIsIndependentOfCapacity(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvMaxSessions: "2", EnvMaxPerClient: "5"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxVMsPerClient != 5 {
		t.Fatalf("MaxVMsPerClient = %d, want 5", cfg.MaxVMsPerClient)
	}
}

func TestDefaultOriginsAreTheDevFrontend(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"localhost:3000", "127.0.0.1:3000"}
	if got := cfg.OriginHosts(); !reflect.DeepEqual(got, want) {
		t.Fatalf("OriginHosts = %v, want %v", got, want)
	}
}

func TestOriginsBecomeHostPatterns(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvAllowedOrigins: " https://Play.Example.com/ , http://localhost:5173 ",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	wantOrigins := []string{"https://play.example.com", "http://localhost:5173"}
	if !reflect.DeepEqual(cfg.AllowedOrigins, wantOrigins) {
		t.Errorf("AllowedOrigins = %v, want %v", cfg.AllowedOrigins, wantOrigins)
	}
	wantHosts := []string{"play.example.com", "localhost:5173"}
	if got := cfg.OriginHosts(); !reflect.DeepEqual(got, wantHosts) {
		t.Errorf("OriginHosts = %v, want %v", got, wantHosts)
	}
}

func TestBadValuesAreRejected(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want string
	}{
		{"wildcard origin", map[string]string{EnvAllowedOrigins: "*"}, EnvAllowedOrigins},
		{"origin without scheme", map[string]string{EnvAllowedOrigins: "localhost:3000"}, EnvAllowedOrigins},
		{"origin with path", map[string]string{EnvAllowedOrigins: "http://a.example/x"}, EnvAllowedOrigins},
		{"empty origin entry", map[string]string{EnvAllowedOrigins: "http://a.example,,http://b.example"}, EnvAllowedOrigins},
		{"address without port", map[string]string{EnvAddr: "localhost"}, EnvAddr},
		{"port out of range", map[string]string{EnvAddr: "127.0.0.1:70000"}, EnvAddr},
		{"memory not a number", map[string]string{EnvVMMemoryMB: "lots"}, EnvVMMemoryMB},
		{"memory too big", map[string]string{EnvVMMemoryMB: "999999"}, EnvVMMemoryMB},
		{"zero sessions", map[string]string{EnvMaxSessions: "0"}, EnvMaxSessions},
		{"zero per client", map[string]string{EnvMaxPerClient: "0"}, EnvMaxPerClient},
		{"per client too big", map[string]string{EnvMaxPerClient: "1001"}, EnvMaxPerClient},
		{"per client not a number", map[string]string{EnvMaxPerClient: "two"}, EnvMaxPerClient},
		{"bad duration", map[string]string{EnvIdleTimeout: "soon"}, EnvIdleTimeout},
		{"negative duration", map[string]string{EnvMaxDuration: "-5m"}, EnvMaxDuration},
		{"max shorter than idle", map[string]string{EnvIdleTimeout: "30m", EnvMaxDuration: "10m"}, EnvMaxDuration},
		{"negative rate limit", map[string]string{EnvRateLimitRPS: "-1"}, EnvRateLimitRPS},
		{"rate without burst", map[string]string{EnvRateLimitBurst: "0"}, EnvRateLimitBurst},
		{"too many proxy hops", map[string]string{EnvTrustedProxyHops: "99"}, EnvTrustedProxyHops},
		{"unknown accelerator", map[string]string{EnvQEMUAccel: "kvn"}, EnvQEMUAccel},
		{"accelerator with injected option", map[string]string{EnvQEMUAccel: "tcg,thread=multi"}, EnvQEMUAccel},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Load error = %v, want one naming %s", err, tt.want)
			}
		})
	}
}

func TestEveryProblemIsReportedAtOnce(t *testing.T) {
	_, err := Load(env(map[string]string{
		EnvAddr:        "nope",
		EnvVMMemoryMB:  "x",
		EnvMaxSessions: "0",
	}))
	if err == nil {
		t.Fatal("Load = nil, want errors")
	}
	for _, key := range []string{EnvAddr, EnvVMMemoryMB, EnvMaxSessions} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error does not mention %s: %v", key, err)
		}
	}
}

func TestValidateChecksImageAndQEMU(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "os.iso")
	if err := os.WriteFile(image, []byte("iso"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(env(map[string]string{EnvImage: image, EnvQEMUBin: "sh"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !filepath.IsAbs(cfg.ImagePath) || !filepath.IsAbs(cfg.QEMUBin) {
		t.Errorf("paths not resolved: image %q, qemu %q", cfg.ImagePath, cfg.QEMUBin)
	}

	missing, err := Load(env(map[string]string{EnvImage: filepath.Join(dir, "gone.iso"), EnvQEMUBin: "no-such-qemu-binary"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	err = missing.Validate()
	if err == nil || !strings.Contains(err.Error(), EnvImage) || !strings.Contains(err.Error(), EnvQEMUBin) {
		t.Fatalf("Validate = %v, want both the image and QEMU reported", err)
	}
}

func TestImageMustBeAFile(t *testing.T) {
	cfg, err := Load(env(map[string]string{EnvImage: t.TempDir(), EnvQEMUBin: "sh"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("Validate accepted a directory as the image")
	}
}

func TestAcceleratorDefaultsToQEMUsChoiceAndIgnoresCase(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QEMUAccel != "" {
		t.Errorf("default QEMUAccel = %q, want empty", cfg.QEMUAccel)
	}

	cfg, err = Load(env(map[string]string{EnvQEMUAccel: " KVM "}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.QEMUAccel != "kvm" {
		t.Errorf("QEMUAccel = %q, want kvm", cfg.QEMUAccel)
	}
}

func TestValidateChecksKVMDeviceOnlyForKVM(t *testing.T) {
	dir := t.TempDir()
	image := filepath.Join(dir, "os.iso")
	device := filepath.Join(dir, "kvm")
	for _, p := range []string{image, device} {
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := kvmDevice
	t.Cleanup(func() { kvmDevice = old })

	validate := func(accel string) error {
		t.Helper()
		cfg, err := Load(env(map[string]string{EnvImage: image, EnvQEMUBin: "sh", EnvQEMUAccel: accel}))
		if err != nil {
			t.Fatal(err)
		}
		return cfg.Validate()
	}

	kvmDevice = device
	if err := validate("kvm"); err != nil {
		t.Errorf("kvm with a usable device: %v", err)
	}

	kvmDevice = filepath.Join(dir, "missing")
	if err := validate("kvm"); err == nil || !strings.Contains(err.Error(), EnvQEMUAccel) {
		t.Errorf("kvm without the device: Validate = %v, want an error naming %s", err, EnvQEMUAccel)
	}
	if err := validate("tcg"); err != nil {
		t.Errorf("tcg must not need the device: %v", err)
	}
}
