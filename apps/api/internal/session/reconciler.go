package session

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	mathrand "math/rand/v2"
	"os"
	"sync"
	"time"

	"github.com/os-playground-api/internal/vm"
)

// Reconciler defaults. Changing sessions are revisited quickly, running ones
// less often; the running interval is also the precision of expiry (ADR
// 0010, section 6.2).
const (
	defaultInterval    = time.Second
	defaultLease       = 30 * time.Second
	defaultBatch       = 8
	defaultStepTimeout = 10 * time.Second
	defaultMaxAttempts = 5
	defaultSweep       = time.Minute

	changingRetry = time.Second
	runningRetry  = 5 * time.Second
	maxBackoff    = time.Minute

	// completeTimeout bounds recording an outcome, even during shutdown: a
	// provider action that already happened should be written down.
	completeTimeout = 5 * time.Second
)

// ReconcilerConfig configures a Reconciler. Store, Provider and
// FailedVisibleFor are required; zero values elsewhere take the defaults.
type ReconcilerConfig struct {
	Store    Store
	Provider vm.Provider

	// FailedVisibleFor is how long a failed or stopped session stays
	// readable after it ended. Its VM is released at once regardless.
	FailedVisibleFor time.Duration

	// Worker names this process in claims; it must be unique among
	// replicas. Empty picks hostname-pid-random.
	Worker string

	Interval    time.Duration // how often to poll for due sessions
	Lease       time.Duration // how long a claim lasts; must exceed StepTimeout
	Batch       int           // sessions claimed, and worked on concurrently, per pass
	StepTimeout time.Duration // bound on the provider calls for one session
	MaxAttempts int           // failed creates before the session is FAILED
	// SweepInterval is how often the orphan sweep runs (ADR 0010, section
	// 6.4). It also runs once when Run starts.
	SweepInterval time.Duration

	Logger *slog.Logger // nil discards
}

// Reconciler moves every session toward its desired state (ADR 0010, section
// 6). It is the only code that creates or deletes VMs. Every step is
// idempotent, so a step repeated after a crash or a lost lease is harmless.
type Reconciler struct {
	store    Store
	provider vm.Provider
	visible  time.Duration
	worker   string
	interval time.Duration
	lease    time.Duration
	batch    int
	timeout  time.Duration
	attempts int
	sweep    time.Duration
	log      *slog.Logger

	now  func() time.Time // replaced in tests
	wake chan struct{}
}

// NewReconciler checks the configuration and fills in defaults.
func NewReconciler(cfg ReconcilerConfig) (*Reconciler, error) {
	var errs []error
	if cfg.Store == nil {
		errs = append(errs, errors.New("session: reconciler Store is required"))
	}
	if cfg.Provider == nil {
		errs = append(errs, errors.New("session: reconciler Provider is required"))
	}
	if cfg.FailedVisibleFor <= 0 {
		errs = append(errs, fmt.Errorf("session: FailedVisibleFor=%s: must be greater than 0", cfg.FailedVisibleFor))
	}
	for name, v := range map[string]time.Duration{
		"Interval": cfg.Interval, "Lease": cfg.Lease, "StepTimeout": cfg.StepTimeout, "SweepInterval": cfg.SweepInterval,
	} {
		if v < 0 {
			errs = append(errs, fmt.Errorf("session: %s=%s: must not be negative", name, v))
		}
	}
	if cfg.Batch < 0 || cfg.MaxAttempts < 0 {
		errs = append(errs, fmt.Errorf("session: Batch=%d, MaxAttempts=%d: must not be negative", cfg.Batch, cfg.MaxAttempts))
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}

	r := &Reconciler{
		store:    cfg.Store,
		provider: cfg.Provider,
		visible:  cfg.FailedVisibleFor,
		worker:   cmp.Or(cfg.Worker, newWorkerID()),
		interval: cmp.Or(cfg.Interval, defaultInterval),
		lease:    cmp.Or(cfg.Lease, defaultLease),
		batch:    cmp.Or(cfg.Batch, defaultBatch),
		timeout:  cmp.Or(cfg.StepTimeout, defaultStepTimeout),
		attempts: cmp.Or(cfg.MaxAttempts, defaultMaxAttempts),
		sweep:    cmp.Or(cfg.SweepInterval, defaultSweep),
		log:      cfg.Logger,
		now:      time.Now,
		wake:     make(chan struct{}, 1),
	}
	if r.lease <= r.timeout {
		return nil, fmt.Errorf("session: Lease=%s must exceed StepTimeout=%s, or a slow step loses its claim", r.lease, r.timeout)
	}
	if r.log == nil {
		r.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return r, nil
}

// newWorkerID is unique per process: the hostname tells an operator where it
// runs, and the random part separates restarts of the same pod.
func newWorkerID() string {
	host, err := os.Hostname()
	if err != nil {
		host = "unknown"
	}
	b := make([]byte, 4)
	_, _ = rand.Read(b) // never fails on supported platforms
	return fmt.Sprintf("%s-%d-%s", host, os.Getpid(), hex.EncodeToString(b))
}

// Wake asks Run for a pass now instead of at the next tick. It never blocks.
func (r *Reconciler) Wake() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run reconciles until ctx ends: at every tick, on every Wake, and again at
// once while passes come back full. It sweeps orphans at start and every
// SweepInterval.
func (r *Reconciler) Run(ctx context.Context) {
	t := time.NewTicker(r.interval)
	defer t.Stop()
	sweep := time.NewTicker(r.sweep)
	defer sweep.Stop()
	r.sweepLogged(ctx)
	for {
		for ctx.Err() == nil {
			n, err := r.ReconcileOnce(ctx)
			if err != nil && ctx.Err() == nil {
				r.log.Error("reconcile pass failed", "err", err)
			}
			if err != nil || n < r.batch {
				break
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-r.wake:
		case <-sweep.C:
			r.sweepLogged(ctx)
		}
	}
}

func (r *Reconciler) sweepLogged(ctx context.Context) {
	if _, err := r.SweepOrphans(ctx); err != nil && ctx.Err() == nil {
		r.log.Error("orphan sweep failed", "err", err)
	}
}

// SweepOrphans deletes every provider VM that no session holds, and returns
// how many it deleted. This is safe because a session row is committed
// before its VM is created, and rows are never hard-deleted: a VM without an
// unreleased row was left behind by something, never about to be used.
func (r *Reconciler) SweepOrphans(ctx context.Context) (int, error) {
	insts, err := r.provider.List(ctx)
	if err != nil {
		return 0, fmt.Errorf("list vms: %w", err)
	}
	if len(insts) == 0 {
		return 0, nil
	}
	ids := make([]string, len(insts))
	for i, inst := range insts {
		ids[i] = inst.ID
	}
	held, err := r.store.HoldingVM(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("holding vm: %w", err)
	}

	deleted := 0
	var errs []error
	for _, id := range ids {
		if held[id] {
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, r.timeout)
		err := r.provider.Delete(dctx, id)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("delete orphan %s: %w", id, err))
			continue
		}
		deleted++
		r.log.Warn("orphan vm deleted", "vm_id", id)
	}
	return deleted, errors.Join(errs...)
}

// ReconcileOnce claims one batch of due sessions, takes one step for each,
// concurrently, and records the outcomes. It returns how many it claimed.
func (r *Reconciler) ReconcileOnce(ctx context.Context) (int, error) {
	claims, err := r.store.Claim(ctx, r.worker, r.lease, r.batch)
	if err != nil {
		return 0, fmt.Errorf("claim: %w", err)
	}

	var wg sync.WaitGroup
	for _, c := range claims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.reconcile(ctx, c)
		}()
	}
	wg.Wait()
	return len(claims), nil
}

func (r *Reconciler) reconcile(ctx context.Context, c Claim) {
	stepCtx, cancel := context.WithTimeout(ctx, r.timeout)
	res := r.step(stepCtx, c)
	cancel()

	// Record even if ctx has ended: the provider may already have acted.
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), completeTimeout)
	defer cancel()
	switch err := r.store.Complete(cctx, c, res); {
	case errors.Is(err, ErrLeaseLost):
		r.log.Warn("reconcile outcome discarded: lease lost", "session_id", c.ID)
	case err != nil:
		r.log.Error("reconcile outcome not recorded", "session_id", c.ID, "err", err)
	case res.Status != "" && res.Status != c.Status:
		r.log.Info("session status changed", "session_id", c.ID, "from", c.Status, "to", res.Status,
			"reason", res.FailureReason)
	}
}

// step takes one step toward the desired state (ADR 0010, section 6.2).
func (r *Reconciler) step(ctx context.Context, c Claim) Result {
	switch {
	case c.DesiredDeleted || c.Status == StatusDeleting:
		return r.remove(ctx, c)
	case c.Status == StatusFailed || c.Status == StatusStopped:
		return r.finished(ctx, c)
	}

	now := r.now()
	if !now.Before(c.ExpiresAt) || !now.Before(c.IdleExpiresAt) {
		reason := "max duration"
		if now.Before(c.ExpiresAt) {
			reason = "idle"
		}
		r.log.Info("session expired", "session_id", c.ID, "reason", reason)
		return Result{DesireDeleted: true, ExpireReason: reason}
	}
	if c.Status == StatusCreated {
		return r.create(ctx, c)
	}
	return r.observe(ctx, c)
}

// create asks the provider for the VM. ErrAlreadyExists means an earlier
// attempt got as far as the provider but not as far as Complete: success.
func (r *Reconciler) create(ctx context.Context, c Claim) Result {
	var spec vmSpec
	if err := json.Unmarshal(c.Spec, &spec); err != nil {
		return Result{Status: StatusFailed, FailureReason: "invalid stored spec: " + err.Error()}
	}

	_, err := r.provider.Create(ctx, vm.Spec{ID: c.ID, Image: c.ImageRef, MemoryMB: spec.MemoryMB})
	switch {
	case err == nil, errors.Is(err, vm.ErrAlreadyExists):
		return Result{Status: StatusProvisioning, RetryAfter: changingRetry}
	case errors.Is(err, vm.ErrInvalidSpec):
		return Result{Status: StatusFailed, FailureReason: err.Error()}
	case c.Attempts+1 >= r.attempts:
		return Result{Status: StatusFailed, FailureReason: "could not create the VM: " + err.Error()}
	default:
		r.log.Warn("create vm failed; will retry", "session_id", c.ID, "attempt", c.Attempts+1, "err", err)
		return r.retry(c)
	}
}

// observe copies the VM's status into the row.
func (r *Reconciler) observe(ctx context.Context, c Claim) Result {
	inst, err := r.provider.Get(ctx, c.ID)
	switch {
	case errors.Is(err, vm.ErrNotFound):
		// Node loss, or a local provider restarted (ADR 0010, section 6.4).
		return Result{Status: StatusFailed, FailureReason: "vm disappeared"}
	case err != nil:
		r.log.Warn("get vm failed; will retry", "session_id", c.ID, "err", err)
		return r.retry(c)
	}

	switch inst.Status {
	case vm.StatusFailed:
		return Result{Status: StatusFailed, FailureReason: cmp.Or(inst.Reason, "vm failed")}
	case vm.StatusStopped:
		return Result{Status: StatusStopped, FailureReason: inst.Reason}
	case vm.StatusProvisioning:
		return Result{Status: StatusProvisioning, RetryAfter: changingRetry}
	case vm.StatusStarting:
		return Result{Status: StatusStarting, RetryAfter: changingRetry}
	case vm.StatusRunning:
		return Result{Status: StatusRunning, RetryAfter: runningRetry}
	default: // unknown: the provider cannot tell right now
		return Result{RetryAfter: changingRetry}
	}
}

// finished releases a failed or stopped session's VM at once, so it frees
// capacity, then keeps the row readable for FailedVisibleFor (ADR 0010,
// section 4). After that it is DELETED; no provider work is left.
func (r *Reconciler) finished(ctx context.Context, c Claim) Result {
	if !c.VMReleased {
		gone, err := r.deleteVM(ctx, c.ID)
		switch {
		case err != nil:
			r.log.Warn("release vm failed; will retry", "session_id", c.ID, "err", err)
			return r.retry(c)
		case !gone:
			return Result{RetryAfter: changingRetry}
		}
		c.VMReleased = true
	}

	ended := c.TerminatedAt
	if ended.IsZero() { // set by Complete; only a hand-edited row lacks it
		ended = r.now()
	}
	if left := ended.Add(r.visible).Sub(r.now()); left > 0 {
		return Result{VMReleased: true, RetryAfter: left}
	}
	return Result{Status: StatusDeleted, DesireDeleted: true, VMReleased: true}
}

// remove deletes the VM of a session marked for deletion.
func (r *Reconciler) remove(ctx context.Context, c Claim) Result {
	if c.VMReleased {
		return Result{Status: StatusDeleted}
	}
	gone, err := r.deleteVM(ctx, c.ID)
	switch {
	case err != nil:
		r.log.Warn("delete vm failed; will retry", "session_id", c.ID, "err", err)
		res := r.retry(c)
		res.Status = StatusDeleting
		return res
	case !gone:
		return Result{Status: StatusDeleting, RetryAfter: changingRetry}
	}
	return Result{Status: StatusDeleted, VMReleased: true}
}

// deleteVM asks for deletion and reports whether the VM is confirmed gone.
// Delete is idempotent, so asking again on every pass is safe.
func (r *Reconciler) deleteVM(ctx context.Context, id string) (bool, error) {
	if err := r.provider.Delete(ctx, id); err != nil {
		return false, err
	}
	_, err := r.provider.Get(ctx, id)
	switch {
	case errors.Is(err, vm.ErrNotFound):
		return true, nil
	case err != nil:
		return false, err
	}
	return false, nil
}

// retry is a failed attempt with exponential backoff and jitter, up to
// maxBackoff.
func (r *Reconciler) retry(c Claim) Result {
	d := changingRetry << min(c.Attempts, 10)
	d = min(d, maxBackoff)
	d = d/2 + mathrand.N(d/2+1) //nolint:gosec // G404: jitter, not a secret
	return Result{Failed: true, RetryAfter: d}
}
