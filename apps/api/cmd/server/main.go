package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/os-playground-api/internal/auth"
	"github.com/os-playground-api/internal/config"
	"github.com/os-playground-api/internal/database"
	"github.com/os-playground-api/internal/httpserver"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/vm/localqemu"
)

// stepTimeout bounds each shutdown step.
const stepTimeout = 5 * time.Second

func main() {
	healthcheck := flag.Bool("healthcheck", false, "ask a running server's /healthz and exit 0 if it answers, 1 if not")
	flag.Parse()

	if *healthcheck {
		if err := runHealthcheck(); err != nil {
			slog.Error("unhealthy", "err", err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

// loadDotEnv reads .env from the working directory. A missing file is normal
// (production sets real variables); anything else is worth saying. Real
// environment variables always win over the file.
func loadDotEnv() {
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not load .env file", "err", err)
	}
}

func run() error {
	loadDotEnv()

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("config loaded", "config", cfg)

	dbCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	// The pool is opened now so a bad URL or an unreachable database fails at
	// startup.
	pool, err := database.New(dbCtx, cfg.DatabaseURL.Reveal(), int32(cfg.DBMaxConns)) //nolint:gosec // G115: Load bounds DBMaxConns to 1..1000
	if err != nil {
		return fmt.Errorf("connect database: %w", err)
	}

	defer pool.Close()

	provider, err := localqemu.New(localqemu.Config{
		QEMUBin: cfg.QEMUBin,
		Options: localqemu.Options{Accelerator: cfg.QEMUAccel},
		MaxVMs:  cfg.MaxSessions,
		Logger:  log,
	})
	if err != nil {
		return err
	}
	// Safety net for early returns. The normal path closes the provider in
	// shutdown, last; Close is safe to call twice.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		if err := provider.Close(ctx); err != nil {
			log.Error("failed to close provider", "err", err)
		}
	}()

	store, err := session.NewPGStore(pool, cfg.AdmissionLockWait)
	if err != nil {
		return err
	}

	logins, err := auth.NewPGStore(pool)
	if err != nil {
		return err
	}
	signIn, err := newSignIn(cfg, logins, log)
	if err != nil {
		return err
	}

	retention, closeRetention, err := newRetention(dbCtx, cfg, store, logins, log)
	if err != nil {
		return err
	}
	defer closeRetention()

	// The reconciler is the only code that creates or deletes VMs (ADR 0010,
	// section 2). Handlers only change rows, then wake it.
	reconciler, err := session.NewReconciler(session.ReconcilerConfig{
		Store:            store,
		Provider:         provider,
		FailedVisibleFor: cfg.FailedVisibleFor,
		Logger:           log,
	})
	if err != nil {
		return err
	}

	manager, err := session.New(session.Config{
		Store:        store,
		Provider:     provider,
		Image:        cfg.ImagePath,
		MemoryMB:     cfg.VMMemoryMB,
		Capacity:     cfg.MaxSessions,
		MaxPerOwner:  cfg.MaxSessionsPerUser,
		MaxPerClient: cfg.MaxSessionsPerClient,
		IdleTimeout:  cfg.IdleTimeout,
		MaxDuration:  cfg.MaxDuration,
		Wake:         reconciler.Wake,
		Logger:       log,
	})
	if err != nil {
		return err
	}

	api := httpserver.NewServer(manager, signIn, pool.Ping, cfg.AllowedOrigins, httpserver.RateLimits{
		RequestsPerSecond: cfg.RateLimitRPS,
		Burst:             cfg.RateLimitBurst,
		CreatesPerMinute:  cfg.CreateLimitPerMinute,
		TrustedProxyHops:  cfg.TrustedProxyHops,
	}, log)
	srv := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Bind before starting anything else: "address already in use" then fails
	// at once with nothing left running.
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	// The reconciler has its own context: shutdown stops it after HTTP, so a
	// DELETE accepted during the drain is still acted on.
	recCtx, stopReconciler := context.WithCancel(context.Background())
	defer stopReconciler()
	// The retention purge shares its context and its shutdown step: both are
	// background database work that HTTP no longer needs once it has drained.
	reconcilerDone := make(chan struct{})
	go func() {
		defer close(reconcilerDone)
		var wg sync.WaitGroup
		wg.Go(func() { reconciler.Run(recCtx) })
		wg.Go(func() { retention.Run(recCtx) })
		wg.Wait()
	}()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String())

	var runErr error
	select {
	case <-ctx.Done():
		log.Info("shutting down")
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			runErr = fmt.Errorf("serve: %w", err)
		}
	}
	// Restores default signal handling, so a second Ctrl-C ends the process at
	// once if shutdown hangs.
	stop()

	err = errors.Join(runErr, shutdown(log, api, srv, provider, stopReconciler, reconcilerDone))
	if err == nil {
		log.Info("stopped")
	}
	return err
}

// shutdown stops everything in dependency order, each step with its own
// timeout, and keeps going when one fails. Terminals are ended first so they
// close cleanly; the reconciler stops after HTTP so requests accepted while
// draining are acted on; the provider goes last because everything above uses it.
func shutdown(log *slog.Logger, api *httpserver.Server, srv *http.Server, provider *localqemu.Provider,
	stopReconciler context.CancelFunc, reconcilerDone <-chan struct{},
) error {
	var errs []error
	step := func(name string, f func(context.Context) error) {
		ctx, cancel := context.WithTimeout(context.Background(), stepTimeout)
		defer cancel()
		if err := f(ctx); err != nil {
			log.Error("shutdown step failed", "step", name, "err", err)
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
		}
	}

	step("terminals", api.Shutdown)
	step("http server", srv.Shutdown)
	step("reconciler and retention", func(ctx context.Context) error {
		stopReconciler()
		select {
		case <-reconcilerDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	step("vm provider", provider.Close)

	return errors.Join(errs...)
}

// newRetention builds the retention purge (ADR 0010, section 12). Audit
// events need a connection that may become playground_retention; without
// PLAYGROUND_RETENTION_DATABASE_URL they are kept, which is said loudly
// because it means the retention policy is not being applied. The returned
// func closes that connection pool.
func newRetention(ctx context.Context, cfg config.Config, store session.Store, logins session.LoginSessionPurger,
	log *slog.Logger,
) (*session.Retention, func(), error) {
	rcfg := session.RetentionConfig{
		Sessions:         store,
		LoginSessions:    logins,
		SessionRetention: cfg.SessionRetention,
		AuditRetention:   cfg.AuditRetention,
		Logger:           log,
	}
	closePool := func() {}
	if url := cfg.RetentionDatabaseURL.Reveal(); url != "" {
		// Two connections: the purge runs one batch at a time.
		rpool, err := database.New(ctx, url, 2)
		if err != nil {
			return nil, nil, fmt.Errorf("connect retention database: %w", err)
		}
		closePool = rpool.Close
		purger, err := session.NewPGAuditPurger(rpool)
		if err != nil {
			rpool.Close()
			return nil, nil, err
		}
		rcfg.Audit = purger
	} else {
		log.Warn("audit events are kept forever: set " + config.EnvRetentionDatabaseURL + " to apply the audit retention period")
	}
	r, err := session.NewRetention(rcfg)
	if err != nil {
		closePool()
		return nil, nil, err
	}
	return r, closePool, nil
}

// newSignIn builds OIDC sign-in, or returns nil when it is not configured:
// the server then runs, but nobody can sign in or start a machine.
func newSignIn(cfg config.Config, store auth.Store, log *slog.Logger) (httpserver.Auth, error) {
	if !cfg.SignInEnabled() {
		log.Warn("sign-in is not configured: set " + config.EnvOIDCClientID + " and " +
			config.EnvOIDCClientSecret + "; until then nobody can start a machine")
		return nil, nil
	}
	s, err := auth.New(auth.Config{
		Issuer:         cfg.OIDCIssuer,
		ClientID:       cfg.OIDCClientID,
		ClientSecret:   cfg.OIDCClientSecret.Reveal(),
		PublicURL:      cfg.PublicURL,
		CallbackPath:   httpserver.CallbackPath,
		AllowedOrigins: cfg.AllowedOrigins,
		SessionTTL:     cfg.LoginSessionTTL,
		Store:          store,
		RequestID:      session.RequestID,
		Logger:         log,
	})
	if err != nil {
		return nil, err
	}
	log.Info("sign-in enabled", "issuer", cfg.OIDCIssuer, "redirect_uri", cfg.PublicURL+httpserver.CallbackPath)
	return s, nil
}
