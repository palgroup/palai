package execution

import (
	"context"
	"time"
)

// ReconcileStore is the coordinator seam the reconciler sweeps through: it dead-letters
// abandoned jobs and drives dead-lettered response.run jobs to a failed run terminal.
// *coordinator.Store implements it; a fake implements it in unit tests.
type ReconcileStore interface {
	ReclaimExpired(ctx context.Context, maxAttempts int) (int, error)
	SweepDeadLetteredRuns(ctx context.Context) (int, error)
}

// Reconciler periodically dead-letters jobs whose lease has lapsed and whose attempts
// are exhausted — the abandoned-work safety net for workers killed every attempt that
// never self-report a terminal failure (spec §24.4) — and then bridges each dead-lettered
// response.run to a failed run terminal so its response never hangs. Expired leases still
// under their attempt ceiling are reclaimed inline by the next claim, so the reconciler
// only enforces the dead-letter ceiling and the terminality bridge.
type Reconciler struct {
	store       ReconcileStore
	interval    time.Duration
	maxAttempts int
}

// NewReconciler binds a sweep interval and attempt ceiling to the store.
func NewReconciler(store ReconcileStore, interval time.Duration, maxAttempts int) *Reconciler {
	return &Reconciler{store: store, interval: interval, maxAttempts: maxAttempts}
}

// Sweep runs one reconciliation pass: it dead-letters abandoned jobs, then drives every
// dead-lettered response.run to a failed run terminal so a run whose every attempt
// violated the protocol reaches terminal rather than hanging in running with an open SSE
// stream (spec §24.4 -> §22.3). It returns the number of jobs dead-lettered this pass.
func (r *Reconciler) Sweep(ctx context.Context) (int, error) {
	dead, err := r.store.ReclaimExpired(ctx, r.maxAttempts)
	if err != nil {
		return dead, err
	}
	if _, err := r.store.SweepDeadLetteredRuns(ctx); err != nil {
		return dead, err
	}
	return dead, nil
}

// Run sweeps every interval until ctx is cancelled. A sweep error is non-fatal: the
// next tick retries, because a transient database blip must not stop the safety net.
func (r *Reconciler) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			_, _ = r.Sweep(ctx)
		}
	}
}
