package execution

import (
	"context"
	"time"
)

// Reclaimer is the coordinator seam the reconciler sweeps through. *coordinator.Store
// implements it; a fake implements it in unit tests.
type Reclaimer interface {
	ReclaimExpired(ctx context.Context, maxAttempts int) (int, error)
}

// Reconciler periodically dead-letters jobs whose lease has lapsed and whose attempts
// are exhausted — the abandoned-work safety net for workers killed every attempt that
// never self-report a terminal failure (spec §24.4). Expired leases still under their
// attempt ceiling are reclaimed inline by the next claim, so the reconciler only
// enforces the dead-letter ceiling.
type Reconciler struct {
	store       Reclaimer
	interval    time.Duration
	maxAttempts int
}

// NewReconciler binds a sweep interval and attempt ceiling to the store.
func NewReconciler(store Reclaimer, interval time.Duration, maxAttempts int) *Reconciler {
	return &Reconciler{store: store, interval: interval, maxAttempts: maxAttempts}
}

// Sweep runs one reconciliation pass and returns the number of jobs dead-lettered.
func (r *Reconciler) Sweep(ctx context.Context) (int, error) {
	return r.store.ReclaimExpired(ctx, r.maxAttempts)
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
