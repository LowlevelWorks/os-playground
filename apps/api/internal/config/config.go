package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	EnvAddr           = "PLAYGROUND_ADDR"
	EnvImage          = "PLAYGROUND_OS_IMAGE"
	EnvQEMUBin        = "PLAYGROUND_QEMU_BIN"
	EnvVMMemoryMB     = "PLAYGROUND_VM_MEMORY_MB"
	EnvMaxSessions    = "PLAYGROUND_MAX_SESSIONS"
	EnvMaxPerClient   = "PLAYGROUND_MAX_VMS_PER_CLIENT"
	EnvAllowedOrigins = "PLAYGROUND_ALLOWED_ORIGINS"
	EnvIdleTimeout    = "PLAYGROUND_IDLE_TIMEOUT"
	EnvMaxDuration    = "PLAYGROUND_MAX_DURATION"

	EnvRateLimitRPS         = "PLAYGROUND_RATE_LIMIT_RPS"
	EnvRateLimitBurst       = "PLAYGROUND_RATE_LIMIT_BURST"
	EnvCreateLimitPerMinute = "PLAYGROUND_CREATE_LIMIT_PER_MINUTE"
	EnvTrustedProxyHops     = "PLAYGROUND_TRUSTED_PROXY_HOPS"
)

const (
	// Loopback only, so the server is reachable from other machines only when
	// someone asks for that explicitly with PLAYGROUND_ADDR.
	defaultAddr         = "127.0.0.1:8080"
	defaultImage        = "build/os.iso"
	defaultQEMUBin      = "qemu-system-x86_64"
	defaultVMMemoryMB   = 128
	defaultMaxSessions  = 2
	defaultMaxPerClient = 1
	defaultIdleTimeout  = 15 * time.Minute
	defaultMaxDuration  = time.Hour

	// Per client. The frontend polls at 2 requests a second while a VM boots.
	defaultRateLimitRPS         = 10
	defaultRateLimitBurst       = 40
	defaultCreateLimitPerMinute = 5

	maxVMMemoryMB  = 4096
	maxMaxSessions = 1000
	maxRateLimit   = 100_000
	maxProxyHops   = 10
)

type Config struct {
	ListenAddr string
	ImagePath  string
	QEMUBin    string
	VMMemoryMB int

	// MaxSessions is how many VMs may exist at once. MaxVMsPerClient is how
	// many of them one client (see TrustedProxyHops) may hold.
	MaxSessions     int
	MaxVMsPerClient int

	// AllowedOrigins are the web pages allowed to open a terminal, as full
	// origins: scheme://host[:port]. Use OriginHosts for the WebSocket handler.
	AllowedOrigins []string

	// IdleTimeout is how long a VM may have no browser attached before it is
	// removed. MaxDuration is the longest any VM may live.
	IdleTimeout time.Duration
	MaxDuration time.Duration

	// Per-client rate limits; 0 turns one off. CreateLimitPerMinute applies
	// to starting VMs on top of the general limit.
	RateLimitRPS         int
	RateLimitBurst       int
	CreateLimitPerMinute int

	// TrustedProxyHops is how many reverse proxies append to X-Forwarded-For.
	// 0 ignores the header and limits by the TCP peer address.
	TrustedProxyHops int
}

type Lookup func(key string) (value string, ok bool)

// Load reads the configuration and reports every problem at once. It does not
// touch the file system: call Validate for that.
func Load(lookup Lookup) (Config, error) {
	l := loader{lookup: lookup}

	cfg := Config{
		ListenAddr:  l.str(EnvAddr, defaultAddr),
		ImagePath:   l.str(EnvImage, defaultImage),
		QEMUBin:     l.str(EnvQEMUBin, defaultQEMUBin),
		VMMemoryMB:  l.intInRange(EnvVMMemoryMB, defaultVMMemoryMB, 1, maxVMMemoryMB),
		MaxSessions: l.intInRange(EnvMaxSessions, defaultMaxSessions, 1, maxMaxSessions),

		MaxVMsPerClient: l.intInRange(EnvMaxPerClient, defaultMaxPerClient, 1, maxMaxSessions),

		RateLimitRPS:         l.intInRange(EnvRateLimitRPS, defaultRateLimitRPS, 0, maxRateLimit),
		RateLimitBurst:       l.intInRange(EnvRateLimitBurst, defaultRateLimitBurst, 0, maxRateLimit),
		CreateLimitPerMinute: l.intInRange(EnvCreateLimitPerMinute, defaultCreateLimitPerMinute, 0, maxRateLimit),
		TrustedProxyHops:     l.intInRange(EnvTrustedProxyHops, 0, 0, maxProxyHops),
	}

	if (cfg.RateLimitRPS == 0) != (cfg.RateLimitBurst == 0) {
		l.fail(EnvRateLimitBurst, strconv.Itoa(cfg.RateLimitBurst),
			"must be 0 exactly when "+EnvRateLimitRPS+" is 0")
	}

	durationsBefore := len(l.errs)
	cfg.IdleTimeout = l.duration(EnvIdleTimeout, defaultIdleTimeout)
	cfg.MaxDuration = l.duration(EnvMaxDuration, defaultMaxDuration)

	if len(l.errs) == durationsBefore && cfg.MaxDuration < cfg.IdleTimeout {
		l.fail(EnvMaxDuration, cfg.MaxDuration.String(),
			"must not be shorter than "+EnvIdleTimeout)
	}

	if err := checkListenAddr(cfg.ListenAddr); err != nil {
		l.fail(EnvAddr, cfg.ListenAddr, err.Error())
	}

	if raw := l.str(EnvAllowedOrigins, ""); raw != "" {
		origins, err := parseOrigins(raw)
		if err != nil {
			l.fail(EnvAllowedOrigins, raw, err.Error())
		} else {
			cfg.AllowedOrigins = origins
		}
	} else {
		cfg.AllowedOrigins = defaultOrigins()
	}

	if len(l.errs) > 0 {
		return Config{}, errors.Join(l.errs...)
	}
	return cfg, nil
}

// Validate checks the things Load cannot: that the image and QEMU exist. It
// replaces ImagePath with an absolute path and QEMUBin with the resolved
// executable, so call it before using either.
func (c *Config) Validate() error {
	var errs []error

	if abs, err := filepath.Abs(c.ImagePath); err != nil {
		errs = append(errs, fmt.Errorf("%s=%q: %w", EnvImage, c.ImagePath, err))
	} else if err := checkReadableFile(abs); err != nil {
		errs = append(errs, fmt.Errorf("%s=%q: %w", EnvImage, c.ImagePath, err))
	} else {
		c.ImagePath = abs
	}

	if bin, err := exec.LookPath(c.QEMUBin); err != nil {
		errs = append(errs, fmt.Errorf("%s=%q: %w", EnvQEMUBin, c.QEMUBin, err))
	} else {
		c.QEMUBin = bin
	}

	return errors.Join(errs...)
}

// OriginHosts returns AllowedOrigins as host patterns ("localhost:3000"),
// which is the form the WebSocket handler matches a browser's Origin against.
// The scheme is not part of that match.
func (c Config) OriginHosts() []string {
	hosts := make([]string, 0, len(c.AllowedOrigins))
	for _, o := range c.AllowedOrigins {
		if u, err := url.Parse(o); err == nil { // already validated by parseOrigins
			hosts = append(hosts, u.Host)
		}
	}
	return hosts
}

func (c Config) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("addr", c.ListenAddr),
		slog.String("image", c.ImagePath),
		slog.String("qemu", c.QEMUBin),
		slog.Int("vm_memory_mb", c.VMMemoryMB),
		slog.Int("max_sessions", c.MaxSessions),
		slog.Int("max_vms_per_client", c.MaxVMsPerClient),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.Duration("idle_timeout", c.IdleTimeout),
		slog.Duration("max_duration", c.MaxDuration),
		slog.Int("rate_limit_rps", c.RateLimitRPS),
		slog.Int("rate_limit_burst", c.RateLimitBurst),
		slog.Int("create_limit_per_minute", c.CreateLimitPerMinute),
		slog.Int("trusted_proxy_hops", c.TrustedProxyHops),
	)
}

func checkReadableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	f, err := os.Open(path) //nolint:gosec // G304: path comes from the operator's config, not from a request
	if err != nil {
		return err
	}
	return f.Close()
}

type loader struct {
	lookup Lookup
	errs   []error
}

func (l *loader) fail(key, raw, msg string) {
	l.errs = append(l.errs, fmt.Errorf("%s=%q: %s", key, raw, msg))
}

func (l *loader) str(key, def string) string {
	if v, ok := l.lookup(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

func (l *loader) intInRange(key string, def, lo, hi int) int {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		l.fail(key, raw, "must be an integer")
		return def
	}
	if n < lo || n > hi {
		l.fail(key, raw, fmt.Sprintf("must be between %d and %d", lo, hi))
		return def
	}
	return n
}

func (l *loader) duration(key string, def time.Duration) time.Duration {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(key, raw, `must be a duration such as "90s", "15m" or "1h"`)
		return def
	}
	if d <= 0 {
		l.fail(key, raw, "must be greater than 0")
		return def
	}
	return d
}

func checkListenAddr(addr string) error {
	_, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("must be host:port, for example %q (%w)", defaultAddr, err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("port %q must be a number between 1 and 65535", portStr)
	}
	return nil
}

// defaultOrigins are the Next.js dev server's addresses. The API's own origin
// needs no entry: same-origin requests are always allowed.
func defaultOrigins() []string {
	return []string{"http://localhost:3000", "http://127.0.0.1:3000"}
}

func parseOrigins(raw string) ([]string, error) {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		entry := strings.TrimRight(strings.TrimSpace(part), "/")
		if entry == "" {
			return nil, errors.New("contains an empty entry")
		}
		if entry == "*" {
			return nil, errors.New(`"*" is not allowed; list explicit origins`)
		}
		u, err := url.Parse(entry)
		if err != nil ||
			(u.Scheme != "http" && u.Scheme != "https") ||
			u.Host == "" || u.User != nil ||
			u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, fmt.Errorf("%q is not a valid origin; want http(s)://host[:port]", entry)
		}
		out = append(out, strings.ToLower(u.Scheme+"://"+u.Host))
	}
	return out, nil
}
