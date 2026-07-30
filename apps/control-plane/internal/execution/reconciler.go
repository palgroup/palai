package execution

import (
	"context"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// ReconcileStore is the coordinator seam the reconciler sweeps through: it dead-letters
// abandoned jobs, drives dead-lettered response.run jobs to a failed run terminal, and expires
// approvals whose minutes-scale deadline elapsed while idle (spec §22.4, E10 T7).
// *coordinator.Store implements it; a fake implements it in unit tests.
type ReconcileStore interface {
	ReclaimExpired(ctx context.Context, maxAttempts int) (int, error)
	SweepDeadLetteredRuns(ctx context.Context) (int, error)
	// SweepExpiredApprovals expires publications whose one-shot approval passed its expiry with no
	// consume observing it (the idle-expiry half; the consume-time guards catch the rest). Returns the
	// number expired this pass.
	SweepExpiredApprovals(ctx context.Context) (int, error)
	// SweepExpiredToolApprovals is the same sweep for a GATED TOOL CALL, and it carries the half that
	// publications never needed: it WAKES the parked run. A publication whose approval lapses leaves
	// nothing waiting — the run finished long before. A tool call's run is parked on the question, so an
	// expiry that only cancelled the call would leave that run waiting forever (E23 T1).
	SweepExpiredToolApprovals(ctx context.Context) (int, error)
	// SweepExpiredCapacityParks ends the runs parked for want of a MACHINE longer than the operator's TTL
	// (E24 T5). ttl <= 0 is a no-op and is the shipped default: there is no honest default duration for
	// "how long until a rented Mac arrives". The projection is the terminal Response body, passed in because
	// it lives in this package with the other terminal projections.
	SweepExpiredCapacityParks(ctx context.Context, ttl time.Duration, projection []byte) (int, error)
	// SweepFinishedBackgroundTasks turns a finished background PROCESS into the model's next turn
	// (E26 T4): each running task row is probed through the observer, and each one that has finished
	// produces EXACTLY ONE notification — enqueued as a background_notice command, and waking the run
	// only if it parked on the task. A nil observer makes it a no-op, which is the honest reading of a
	// deployment with no background runner wired.
	SweepFinishedBackgroundTasks(ctx context.Context, observe coordinator.BackgroundObserver) (int, error)
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
	// capacityParkTTL bounds how long a run may sit parked for want of a MACHINE (E24 T5). ZERO IS THE
	// DEFAULT AND IT MEANS NEVER, deliberately: a default here would be a guess about how long a rented Mac
	// takes to arrive, and AWS's own documented range for starting a Mac host is 6 to 20 minutes.
	capacityParkTTL time.Duration
	// background probes one background task's handle and reads the bounded tail of its output (E26 T4).
	// NIL IS THE DEFAULT AND IT MEANS "THIS DEPLOYMENT STARTS NO BACKGROUND TASKS": the observer is the
	// orchestrator's, and an orchestrator with no background runner wired hands back nil rather than a
	// probe that would answer wrongly. The shipped compose file sets neither PALAI_SANDBOX_IMAGE nor
	// PALAI_SHELL_NATIVE, so there is no shell tool there at all and nothing to sweep.
	background coordinator.BackgroundObserver
}

// NewReconciler binds a sweep interval and attempt ceiling to the store.
func NewReconciler(store ReconcileStore, interval time.Duration, maxAttempts int) *Reconciler {
	return &Reconciler{store: store, interval: interval, maxAttempts: maxAttempts}
}

// WithCapacityParkTTL opts the deployment into expiring capacity parks (PALAI_FLEET_PARK_TTL). A setter
// rather than a constructor parameter so every existing caller compiles unchanged, and because the honest
// default is "off" — see the field.
func (r *Reconciler) WithCapacityParkTTL(ttl time.Duration) *Reconciler {
	r.capacityParkTTL = ttl
	return r
}

// WithBackgroundTasks opts the deployment into the exit-notification sweep (E26 T4). A setter for the
// same two reasons WithCapacityParkTTL is one: every existing caller compiles unchanged, and the honest
// default is off — a control plane that cannot start a background task has none to notice finishing.
func (r *Reconciler) WithBackgroundTasks(observe coordinator.BackgroundObserver) *Reconciler {
	r.background = observe
	return r
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
	// Expire approvals whose minutes-scale deadline elapsed while idle (spec §22.4, E10 T7): a
	// forward-declared expiry that no approve/publish consume ever observed. Non-fatal like the rest of
	// the pass — the next tick retries — so a transient blip never stops the dead-letter safety net.
	if _, err := r.store.SweepExpiredApprovals(ctx); err != nil {
		return dead, err
	}
	// And the gated-tool half, which RELEASES A PARKED RUN as well as cancelling the call. Non-fatal like
	// the rest of the pass; a transient blip means the next tick frees the run instead of this one.
	if _, err := r.store.SweepExpiredToolApprovals(ctx); err != nil {
		return dead, err
	}
	// And the CAPACITY half (E24 T5): a run parked because its pool held no machine, for longer than the
	// operator allowed. It rides this loop rather than a reaper of its own because this loop already exists,
	// already spans tenants, and is already supervised — and because an unconfigured TTL makes the call a
	// return-immediately no-op, so a deployment that opts out pays one comparison per tick.
	if _, err := r.store.SweepExpiredCapacityParks(ctx, r.capacityParkTTL, capacityTimeoutProjection); err != nil {
		return dead, err
	}
	// And the BACKGROUND half (E26 T4): a process that outlived the attempt that started it, and has now
	// finished. It rides this loop for the reason the capacity sweep rides it — the loop exists, spans
	// tenants and is supervised — and it is the ONLY path by which an exit reaches a model, which makes
	// `PALAI_DISPATCH_WORKERS=0` (main.go's early return, and the shipped compose file's own value) a
	// deployment where no notification ever lands. That is written down in
	// docs/operations/background-execution.md rather than left to be discovered.
	//
	// Non-fatal like the rest of the pass: a Docker socket that blinked means the next tick delivers the
	// notification instead of this one, and the exactly-once claim is a row, so nothing is delivered twice.
	if _, err := r.store.SweepFinishedBackgroundTasks(ctx, r.background); err != nil {
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
