package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"
	"github.com/os-playground-api/internal/config"
	"github.com/os-playground-api/internal/httpserver"
	"github.com/os-playground-api/internal/session"
	"github.com/os-playground-api/internal/vm/localqemu"
)

const (
	reapInterval = 15 * time.Second
	stepTimeout  = 5 * time.Second // each shutdown step gets its own
)

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	// A missing .env is normal (production sets real variables); anything else
	// is worth saying. Real environment variables always win over the file.
	if err := godotenv.Load(); err != nil && !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("could not load .env file", "err", err)
	}

	cfg, err := config.Load(os.LookupEnv)
	if err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	log.Info("config loaded", "config", cfg)

	provider, err := localqemu.New(localqemu.Config{
		QEMUBin: cfg.QEMUBin,
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

	manager, err := session.New(session.Config{
		Provider:    provider,
		Image:       cfg.ImagePath,
		MemoryMB:    cfg.VMMemoryMB,
		IdleTimeout: cfg.IdleTimeout,
		MaxDuration: cfg.MaxDuration,
		Logger:      log,
	})
	if err != nil {
		return err
	}

	api := httpserver.NewServer(manager, cfg.AllowedOrigins, log)
	srv := &http.Server{
		Handler:           api,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Bind before starting anything else: "address already in use" then fails
	// at once with nothing left running.
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reaperDone := make(chan struct{})
	go func() {
		defer close(reaperDone)
		manager.Run(ctx, reapInterval)
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
	// once if shutdown hangs. It also stops the reaper if we got here by error.
	stop()

	err = errors.Join(runErr, shutdown(log, api, srv, provider, reaperDone))
	if err == nil {
		log.Info("stopped")
	}
	return err
}

// shutdown stops everything in dependency order, each step with its own
// timeout, and keeps going when one fails. Terminals are ended first so they
// close cleanly; the provider goes last because everything above uses it.
func shutdown(log *slog.Logger, api *httpserver.Server, srv *http.Server, provider *localqemu.Provider, reaperDone <-chan struct{}) error {
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
	step("session reaper", func(ctx context.Context) error {
		select {
		case <-reaperDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	step("vm provider", provider.Close)

	return errors.Join(errs...)
}
