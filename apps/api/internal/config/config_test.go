package config

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testDatabaseURL is a valid URL every test gets unless it sets its own.
const testDatabaseURL = "postgres://playground:s3cret@127.0.0.1:5432/playground"

// env serves m, plus testDatabaseURL when m does not mention the key. Set the
// key to "" to test it missing.
func env(m map[string]string) Lookup {
	return func(k string) (string, bool) {
		v, ok := m[k]
		if !ok && k == EnvDatabaseURL {
			return testDatabaseURL, true
		}
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("ListenAddr = %q, want loopback only", cfg.ListenAddr)
	}
	if cfg.VMMemoryMB != 128 || cfg.MaxSessions != 2 {
		t.Errorf("memory/sessions = %d/%d, want 128/2", cfg.VMMemoryMB, cfg.MaxSessions)
	}
	// ADR 0010, section 13.3.
	if cfg.MaxSessionsPerUser != 1 || cfg.MaxSessionsPerClient != 3 || cfg.FailedVisibleFor != 5*time.Minute {
		t.Errorf("per user/per client/failed visible = %d/%d/%s, want 1/3/5m",
			cfg.MaxSessionsPerUser, cfg.MaxSessionsPerClient, cfg.FailedVisibleFor)
	}
	if cfg.IdleTimeout != 15*time.Minute || cfg.MaxDuration != time.Hour {
		t.Errorf("timeouts = %s/%s", cfg.IdleTimeout, cfg.MaxDuration)
	}
	if cfg.RateLimitRPS == 0 || cfg.CreateLimitPerMinute == 0 || cfg.TrustedProxyHops != 0 {
		t.Errorf("rate limits = %d rps, %d creates/min, %d hops; want limits on and no trusted proxy",
			cfg.RateLimitRPS, cfg.CreateLimitPerMinute, cfg.TrustedProxyHops)
	}
}

// The limits are independent: one that exceeds another never bites, which is
// a valid choice (a single-user deployment, or a per-user limit above the
// per-client one on a network known to be one person).
func TestLimitsAreIndependent(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		EnvMaxSessions: "2", EnvMaxSessionsPerUser: "4", EnvMaxSessionsPerClient: "5",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.MaxSessionsPerUser != 4 || cfg.MaxSessionsPerClient != 5 {
		t.Fatalf("per user/per client = %d/%d, want 4/5", cfg.MaxSessionsPerUser, cfg.MaxSessionsPerClient)
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
		{"zero per user", map[string]string{EnvMaxSessionsPerUser: "0"}, EnvMaxSessionsPerUser},
		{"per user not a number", map[string]string{EnvMaxSessionsPerUser: "one"}, EnvMaxSessionsPerUser},
		{"zero per client", map[string]string{EnvMaxSessionsPerClient: "0"}, EnvMaxSessionsPerClient},
		{"per client too big", map[string]string{EnvMaxSessionsPerClient: "1001"}, EnvMaxSessionsPerClient},
		{"per client not a number", map[string]string{EnvMaxSessionsPerClient: "two"}, EnvMaxSessionsPerClient},
		{"failed visible zero", map[string]string{EnvFailedVisibleFor: "0s"}, EnvFailedVisibleFor},
		{"failed visible too long", map[string]string{EnvFailedVisibleFor: "48h"}, EnvFailedVisibleFor},
		{"bad duration", map[string]string{EnvIdleTimeout: "soon"}, EnvIdleTimeout},
		{"negative duration", map[string]string{EnvMaxDuration: "-5m"}, EnvMaxDuration},
		{"max shorter than idle", map[string]string{EnvIdleTimeout: "30m", EnvMaxDuration: "10m"}, EnvMaxDuration},
		{"negative rate limit", map[string]string{EnvRateLimitRPS: "-1"}, EnvRateLimitRPS},
		{"rate without burst", map[string]string{EnvRateLimitBurst: "0"}, EnvRateLimitBurst},
		{"too many proxy hops", map[string]string{EnvTrustedProxyHops: "99"}, EnvTrustedProxyHops},
		{"unknown accelerator", map[string]string{EnvQEMUAccel: "kvn"}, EnvQEMUAccel},
		{"accelerator with injected option", map[string]string{EnvQEMUAccel: "tcg,thread=multi"}, EnvQEMUAccel},
		{"database url missing", map[string]string{EnvDatabaseURL: ""}, EnvDatabaseURL},
		{"database url blank", map[string]string{EnvDatabaseURL: "   "}, EnvDatabaseURL},
		{"database url wrong scheme", map[string]string{EnvDatabaseURL: "mysql://u:p@h/db"}, EnvDatabaseURL},
		{"database url without host", map[string]string{EnvDatabaseURL: "postgres://u:p@/db"}, EnvDatabaseURL},
		{"database url without user", map[string]string{EnvDatabaseURL: "postgres://h/db"}, EnvDatabaseURL},
		{"database url without name", map[string]string{EnvDatabaseURL: "postgres://u:p@h/"}, EnvDatabaseURL},
		{"database url bad port", map[string]string{EnvDatabaseURL: "postgres://u:p@h:0/db"}, EnvDatabaseURL},
		{"database url unparsable", map[string]string{EnvDatabaseURL: "postgres://u:p@h:x y/db"}, EnvDatabaseURL},
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

func TestDatabaseURLIsKept(t *testing.T) {
	for _, raw := range []string{testDatabaseURL, "postgresql://u@db.internal/app?sslmode=require"} {
		cfg, err := Load(env(map[string]string{EnvDatabaseURL: raw}))
		if err != nil {
			t.Fatalf("Load(%q): %v", raw, err)
		}
		if cfg.DatabaseURL.Reveal() != raw {
			t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, raw)
		}
	}
}

// The error is logged at startup, so it must never carry the password.
func TestDatabaseURLErrorHidesPassword(t *testing.T) {
	for _, raw := range []string{
		"mysql://u:hunter2@h/db",        // fails validation
		"postgres://u:hunter2@h:x y/db", // fails to parse
	} {
		_, err := Load(env(map[string]string{EnvDatabaseURL: raw}))
		if err == nil {
			t.Fatalf("Load(%q) = nil, want an error", raw)
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("error leaks the password: %v", err)
		}
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

// A Secret must not leak through any of the ways a Config gets printed.
func TestDatabaseURLNeverPrinted(t *testing.T) {
	const pw = "s3cret"
	for _, raw := range []string{
		testDatabaseURL,
		"postgres://u@h/db?password=" + pw,
	} {
		cfg, err := Load(env(map[string]string{EnvDatabaseURL: raw}))
		if err != nil {
			t.Fatalf("Load(%q): %v", raw, err)
		}
		var logs strings.Builder
		slog.New(slog.NewTextHandler(&logs, nil)).Info("x", "config", cfg)
		js, _ := json.Marshal(cfg)
		for name, out := range map[string]string{
			"%v":   fmt.Sprintf("%v", cfg),
			"%+v":  fmt.Sprintf("%+v", cfg),
			"%#v":  fmt.Sprintf("%#v", cfg),
			"json": string(js),
			"slog": logs.String(),
		} {
			if strings.Contains(out, pw) {
				t.Errorf("%s of a Config shows the password: %s", name, out)
			}
		}
		if !strings.Contains(logs.String(), "h") || !strings.Contains(logs.String(), "db") {
			t.Errorf("slog should still show host and database: %s", logs.String())
		}
	}
}

func TestDatabaseURLForms(t *testing.T) {
	ok := []string{
		"postgres://u@/db?host=/var/run/postgresql",
		"postgres://u:p@h/db?sslmode=verify-full",
	}
	for _, raw := range ok {
		if _, err := Load(env(map[string]string{EnvDatabaseURL: raw})); err != nil {
			t.Errorf("Load(%q) = %v, want ok", raw, err)
		}
	}
	_, err := Load(env(map[string]string{EnvDatabaseURL: "postgres://u:p@h/db?sslmode=requir&password=hunter2"}))
	if err == nil || !strings.Contains(err.Error(), "sslmode") {
		t.Fatalf("bad sslmode: err = %v", err)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error shows a ?password= value: %v", err)
	}
}

// One bad value must produce one error, not a second one comparing the
// default it fell back to with a related setting.
func TestBadValueDoesNotReportAMadeUpConflict(t *testing.T) {
	_, err := Load(env(map[string]string{EnvRateLimitRPS: "abc", EnvRateLimitBurst: "0"}))
	if err == nil {
		t.Fatal("Load = nil, want an error")
	}
	if strings.Contains(err.Error(), EnvRateLimitBurst) {
		t.Errorf("reports a conflict caused only by the fallback default: %v", err)
	}
}

func TestDatabaseSettings(t *testing.T) {
	cfg, err := Load(env(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.DBMaxConns != defaultDBMaxConns || cfg.AdmissionLockWait != defaultAdmissionLockWait {
		t.Errorf("defaults = %d, %v", cfg.DBMaxConns, cfg.AdmissionLockWait)
	}
	for _, bad := range []map[string]string{
		{EnvDBMaxConns: "0"},
		{EnvAdmissionLockWait: "0s"},
		{EnvAdmissionLockWait: "1m"},
	} {
		if _, err := Load(env(bad)); err == nil {
			t.Errorf("Load(%v) = nil, want an error", bad)
		}
	}
}
