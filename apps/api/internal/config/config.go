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
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	EnvAddr           = "PLAYGROUND_ADDR"
	EnvMetricsAddr    = "PLAYGROUND_METRICS_ADDR"
	EnvImage          = "PLAYGROUND_OS_IMAGE"
	EnvQEMUBin        = "PLAYGROUND_QEMU_BIN"
	EnvQEMUAccel      = "PLAYGROUND_QEMU_ACCEL"
	EnvVMMemoryMB     = "PLAYGROUND_VM_MEMORY_MB"
	EnvMaxSessions    = "PLAYGROUND_MAX_SESSIONS"
	EnvAllowedOrigins = "PLAYGROUND_ALLOWED_ORIGINS"
	EnvIdleTimeout    = "PLAYGROUND_IDLE_TIMEOUT"
	EnvMaxDuration    = "PLAYGROUND_MAX_DURATION"

	EnvMaxSessionsPerUser   = "PLAYGROUND_MAX_SESSIONS_PER_USER"
	EnvMaxSessionsPerClient = "PLAYGROUND_MAX_SESSIONS_PER_CLIENT"
	EnvFailedVisibleFor     = "PLAYGROUND_FAILED_VISIBLE_FOR"

	EnvRateLimitRPS         = "PLAYGROUND_RATE_LIMIT_RPS"
	EnvRateLimitBurst       = "PLAYGROUND_RATE_LIMIT_BURST"
	EnvCreateLimitPerMinute = "PLAYGROUND_CREATE_LIMIT_PER_MINUTE"
	EnvTrustedProxyHops     = "PLAYGROUND_TRUSTED_PROXY_HOPS"

	EnvDatabaseURL       = "PLAYGROUND_DATABASE_URL"
	EnvDBMaxConns        = "PLAYGROUND_DB_MAX_CONNS"
	EnvAdmissionLockWait = "PLAYGROUND_ADMISSION_LOCK_WAIT"
)

const (
	// Loopback only, so the server is reachable from other machines only when
	// someone asks for that explicitly with PLAYGROUND_ADDR.
	defaultAddr        = "127.0.0.1:8080"
	defaultMetricsAddr = "127.0.0.1:9090"
	defaultImage       = "build/os.iso"
	defaultQEMUBin     = "qemu-system-x86_64"
	defaultVMMemoryMB  = 128
	defaultMaxSessions = 2
	// ADR 0010, section 13.3: one live session per user, and a looser
	// per-address backstop so shared networks (offices, schools) still work.
	defaultMaxSessionsPerUser   = 1
	defaultMaxSessionsPerClient = 3
	defaultFailedVisibleFor     = 5 * time.Minute
	defaultIdleTimeout          = 15 * time.Minute
	defaultMaxDuration          = time.Hour

	// Per client. The frontend polls at 2 requests a second while a VM boots.
	defaultRateLimitRPS         = 10
	defaultRateLimitBurst       = 40
	defaultCreateLimitPerMinute = 5

	// Admission holds one pooled connection per waiting create, so the pool
	// must be larger than the create burst the rate limiter lets through.
	defaultDBMaxConns        = 10
	defaultAdmissionLockWait = 2 * time.Second

	maxVMMemoryMB = 4096
	maxDBMaxConns = 1000
	maxLockWait   = 30 * time.Second
	maxLifetime   = 7 * 24 * time.Hour
	// A failed session holds no VM, but it does keep a row and a page alive.
	maxFailedVisible = 24 * time.Hour
	maxMaxSessions   = 1000
	maxRateLimit     = 100_000
	maxProxyHops     = 10
)

// accelerators are the values localqemu accepts. It checks them again; they
// are checked here too so a typo is reported with the setting's name.
var accelerators = []string{"tcg", "kvm", "hvf"}

type Config struct {
	ListenAddr string

	// MetricsAddr serves Prometheus metrics at /metrics. It is a listener of
	// its own, never the API's: nothing public should reach it.
	MetricsAddr string

	// DatabaseURL is a Secret: printing, logging or JSON-encoding the Config
	// never shows it. Call Reveal to pass it to the driver.
	DatabaseURL Secret

	// DBMaxConns caps the connection pool. AdmissionLockWait is how long a
	// create waits for the admission lock before answering 503.
	DBMaxConns        int
	AdmissionLockWait time.Duration

	ImagePath  string
	QEMUBin    string
	VMMemoryMB int

	// QEMUAccel is "", "tcg", "kvm" or "hvf"; empty leaves the choice to QEMU,
	// which picks tcg. kvm needs Linux with /dev/kvm, which Validate checks.
	QEMUAccel string

	// MaxSessions is how many VMs may exist at once. MaxSessionsPerUser is how
	// many live sessions one owner may hold (until login, an owner is the
	// browser's anonymous token). MaxSessionsPerClient is the backstop for one
	// network address (see TrustedProxyHops), across all of its owners.
	MaxSessions          int
	MaxSessionsPerUser   int
	MaxSessionsPerClient int

	// FailedVisibleFor is how long a failed or stopped session stays readable
	// after it ends, so the page can show why (ADR 0010, section 4). Its VM
	// and its quota are released at once. Used by the reconciler (stage 3.4).
	FailedVisibleFor time.Duration

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
	l := loader{lookup: lookup, bad: map[string]bool{}}

	cfg := Config{
		ListenAddr:  l.str(EnvAddr, defaultAddr),
		MetricsAddr: l.str(EnvMetricsAddr, defaultMetricsAddr),

		ImagePath:  l.str(EnvImage, defaultImage),
		QEMUBin:    l.str(EnvQEMUBin, defaultQEMUBin),
		QEMUAccel:  l.oneOf(EnvQEMUAccel, "", accelerators),
		VMMemoryMB: l.intInRange(EnvVMMemoryMB, defaultVMMemoryMB, 1, maxVMMemoryMB),

		MaxSessions:          l.intInRange(EnvMaxSessions, defaultMaxSessions, 1, maxMaxSessions),
		MaxSessionsPerUser:   l.intInRange(EnvMaxSessionsPerUser, defaultMaxSessionsPerUser, 1, maxMaxSessions),
		MaxSessionsPerClient: l.intInRange(EnvMaxSessionsPerClient, defaultMaxSessionsPerClient, 1, maxMaxSessions),
		FailedVisibleFor:     l.duration(EnvFailedVisibleFor, defaultFailedVisibleFor, maxFailedVisible),

		IdleTimeout: l.duration(EnvIdleTimeout, defaultIdleTimeout, maxLifetime),
		MaxDuration: l.duration(EnvMaxDuration, defaultMaxDuration, maxLifetime),

		RateLimitRPS:         l.intInRange(EnvRateLimitRPS, defaultRateLimitRPS, 0, maxRateLimit),
		RateLimitBurst:       l.intInRange(EnvRateLimitBurst, defaultRateLimitBurst, 0, maxRateLimit),
		CreateLimitPerMinute: l.intInRange(EnvCreateLimitPerMinute, defaultCreateLimitPerMinute, 0, maxRateLimit),
		TrustedProxyHops:     l.intInRange(EnvTrustedProxyHops, 0, 0, maxProxyHops),

		DBMaxConns:        l.intInRange(EnvDBMaxConns, defaultDBMaxConns, 1, maxDBMaxConns),
		AdmissionLockWait: l.duration(EnvAdmissionLockWait, defaultAdmissionLockWait, maxLockWait),
	}

	l.listenAddrs(&cfg)
	cfg.AllowedOrigins = l.origins()
	cfg.DatabaseURL = l.databaseURL()
	l.relations(cfg)

	if len(l.errs) > 0 {
		return Config{}, errors.Join(l.errs...)
	}
	return cfg, nil
}

func (l *loader) listenAddrs(cfg *Config) {
	if err := checkListenAddr(cfg.ListenAddr); err != nil {
		l.fail(EnvAddr, cfg.ListenAddr, err.Error())
	}
	if err := checkListenAddr(cfg.MetricsAddr); err != nil {
		l.fail(EnvMetricsAddr, cfg.MetricsAddr, err.Error())
	} else if l.ok(EnvAddr) && cfg.MetricsAddr == cfg.ListenAddr {
		l.fail(EnvMetricsAddr, cfg.MetricsAddr, "must differ from "+EnvAddr)
	}
}

func (l *loader) origins() []string {
	raw := l.str(EnvAllowedOrigins, "")
	if raw == "" {
		return defaultOrigins()
	}
	origins, err := parseOrigins(raw)
	if err != nil {
		l.fail(EnvAllowedOrigins, raw, err.Error())
		return nil
	}
	return origins
}

func (l *loader) databaseURL() Secret {
	raw := l.str(EnvDatabaseURL, "")
	if raw == "" {
		l.fail(EnvDatabaseURL, "", "is required")
		return ""
	}
	if err := validateDatabaseURL(raw); err != nil {
		// The URL holds the password and this error is logged: never echo it.
		l.fail(EnvDatabaseURL, redactURL(raw), err.Error())
		return ""
	}
	return Secret(raw)
}

// relations checks rules between settings. Each runs only when the settings it
// compares parsed, so one bad value does not also report a made-up conflict
// with the default that replaced it.
func (l *loader) relations(cfg Config) {
	if l.ok(EnvRateLimitRPS, EnvRateLimitBurst) &&
		(cfg.RateLimitRPS == 0) != (cfg.RateLimitBurst == 0) {
		l.fail(EnvRateLimitBurst, strconv.Itoa(cfg.RateLimitBurst),
			"must be 0 exactly when "+EnvRateLimitRPS+" is 0")
	}
	if l.ok(EnvIdleTimeout, EnvMaxDuration) && cfg.MaxDuration < cfg.IdleTimeout {
		l.fail(EnvMaxDuration, cfg.MaxDuration.String(),
			"must not be shorter than "+EnvIdleTimeout)
	}
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

	// Without this, a missing or unwritable /dev/kvm (in a container: no
	// --device, or no --group-add for its group) only shows up as the first
	// user's VM failing to start.
	if c.QEMUAccel == "kvm" {
		if err := checkReadWritable(kvmDevice); err != nil {
			errs = append(errs, fmt.Errorf("%s=%q: %w", EnvQEMUAccel, c.QEMUAccel, err))
		}
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
		slog.String("metrics_addr", c.MetricsAddr),
		slog.Any("database", c.DatabaseURL),
		slog.Int("db_max_conns", c.DBMaxConns),
		slog.Duration("admission_lock_wait", c.AdmissionLockWait),
		slog.String("image", c.ImagePath),
		slog.String("qemu", c.QEMUBin),
		slog.String("qemu_accel", c.QEMUAccel),
		slog.Int("vm_memory_mb", c.VMMemoryMB),
		slog.Int("max_sessions", c.MaxSessions),
		slog.Int("max_sessions_per_user", c.MaxSessionsPerUser),
		slog.Int("max_sessions_per_client", c.MaxSessionsPerClient),
		slog.Duration("failed_visible_for", c.FailedVisibleFor),
		slog.Any("allowed_origins", c.AllowedOrigins),
		slog.Duration("idle_timeout", c.IdleTimeout),
		slog.Duration("max_duration", c.MaxDuration),
		slog.Int("rate_limit_rps", c.RateLimitRPS),
		slog.Int("rate_limit_burst", c.RateLimitBurst),
		slog.Int("create_limit_per_minute", c.CreateLimitPerMinute),
		slog.Int("trusted_proxy_hops", c.TrustedProxyHops),
	)
}

// kvmDevice is a variable so tests can point it at a file of their own.
var kvmDevice = "/dev/kvm"

func checkReadWritable(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0) //nolint:gosec // G304: path is kvmDevice, not user input
	if err != nil {
		return err
	}
	return f.Close()
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
	bad    map[string]bool // keys that failed, so relations can skip them
}

func (l *loader) fail(key, raw, msg string) {
	l.bad[key] = true
	l.errs = append(l.errs, fmt.Errorf("%s=%q: %s", key, raw, msg))
}

// ok reports whether none of keys has failed.
func (l *loader) ok(keys ...string) bool {
	for _, k := range keys {
		if l.bad[k] {
			return false
		}
	}
	return true
}

func (l *loader) str(key, def string) string {
	if v, ok := l.lookup(key); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return def
}

// oneOf reads a value that must be one of allowed, ignoring case.
func (l *loader) oneOf(key, def string, allowed []string) string {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	v := strings.ToLower(raw)
	if !slices.Contains(allowed, v) {
		l.fail(key, raw, "must be one of "+strings.Join(allowed, ", "))
		return def
	}
	return v
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

func (l *loader) duration(key string, def, hi time.Duration) time.Duration {
	raw := l.str(key, "")
	if raw == "" {
		return def
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		l.fail(key, raw, `must be a duration such as "90s", "15m" or "1h"`)
		return def
	}
	if d <= 0 || d > hi {
		l.fail(key, raw, "must be greater than 0 and at most "+hi.String())
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

// sslModes are the values libpq and pgx accept; a typo is otherwise only
// found at the first connection.
var sslModes = []string{"disable", "allow", "prefer", "require", "verify-ca", "verify-full"}

// validateDatabaseURL accepts what pgx accepts in URL form, including a Unix
// socket given as ?host=/path. Its errors never contain the URL.
func validateDatabaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		// Not %w: url.Error quotes the whole URL, password included.
		return errors.New("not a valid URL")
	}
	if u.Scheme != "postgres" && u.Scheme != "postgresql" {
		return errors.New("scheme must be postgres or postgresql")
	}
	q := u.Query()
	if u.Hostname() == "" && q.Get("host") == "" {
		return errors.New("a host is required, in the URL or as ?host=/socket/dir")
	}
	if u.User == nil || u.User.Username() == "" {
		return errors.New("username is required")
	}
	if strings.Trim(u.Path, "/") == "" {
		return errors.New("database name is required")
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
			return errors.New("port must be between 1 and 65535")
		}
	}
	if mode := q.Get("sslmode"); mode != "" && !slices.Contains(sslModes, mode) {
		return errors.New("sslmode must be one of " + strings.Join(sslModes, ", "))
	}
	return nil
}

// redactURL is raw with any password replaced, whether in the userinfo or as
// ?password=, or a placeholder when raw does not parse and so cannot be
// redacted safely.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparsable>"
	}
	if q := u.Query(); q.Has("password") {
		q.Set("password", "xxxxx")
		u.RawQuery = q.Encode()
	}
	return u.Redacted()
}
