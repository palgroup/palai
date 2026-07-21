package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the uncertain-tool reconciliation loop (spec §26.7, E10 T7): the supervised safety net
// that resolves tool_calls stuck `uncertain` by a kill-between-execute-and-commit. It is the reconcile
// half of the durable tool ledger — dispatchTool writes the `uncertain` row and STOPS the run; this loop
// drives each to one of the three §26.7 exits and re-enqueues the run so a fresh attempt continues. It
// reuses the retention/GC supervised-loop shape (reconciler.go), not a new framework.
//
// The three exits (spec §26.7):
//   - irreversible / interactive → manual_resolution: a human must decide; NEVER auto-resolved, and the
//     run stays STOPPED (not re-enqueued) until resolved (TOL-003).
//   - reversible → probe the destination (the tool's own read surface) and, per policy, reconciled_completed
//     (the effect landed — its result re-enters reasoning) or reconciled_not_applied (it did not — a typed
//     not-applied result); then re-enqueue the run (TOL-004).
//   - idempotent uncertain (should be rare — idempotent normally re-executes) is treated as reversible.

// ToolDestinationProber queries a tool's DESTINATION to decide whether an uncertain effect actually
// landed (spec §26.7, fork 5: the tool's own read surface, e.g. a GET against the same endpoint — NOT a
// generic prober framework). Returns applied + the result to record when applied. A tool with no probe
// surface returns supported=false, so the loop escalates to manual_resolution rather than guessing.
type ToolDestinationProber interface {
	Probe(ctx context.Context, call coordinator.UncertainToolCall) (applied bool, result []byte, supported bool, err error)
}

// UncertainReconcileStore is the coordinator seam the reconcile loop drives (the ReconcileStore idiom):
// read the uncertain set, resolve each to an exit, and re-enqueue the run. *coordinator.Store implements
// it; a fake implements it in the deterministic test.
type UncertainReconcileStore interface {
	UncertainToolCalls(ctx context.Context, limit int) ([]coordinator.UncertainToolCall, error)
	ReconcileToolCall(ctx context.Context, tenant coordinator.Tenant, sessionID, responseID, runID, callID, resolution string, result []byte) error
	ReenqueueResponseRun(ctx context.Context, tenant coordinator.Tenant, runID string) error
}

// UncertainReconciler resolves uncertain tool_calls on a supervised interval. batch bounds one pass.
type UncertainReconciler struct {
	store    UncertainReconcileStore
	prober   ToolDestinationProber
	interval time.Duration
	batch    int
}

// NewUncertainReconciler binds the store, the destination prober, and the sweep interval/batch.
func NewUncertainReconciler(store UncertainReconcileStore, prober ToolDestinationProber, interval time.Duration, batch int) *UncertainReconciler {
	if batch <= 0 {
		batch = 100
	}
	return &UncertainReconciler{store: store, prober: prober, interval: interval, batch: batch}
}

// Sweep runs one reconciliation pass and returns the number of uncertain tool_calls resolved. Each is
// driven to its §26.7 exit; a run whose call reached a reconciled_* exit is re-enqueued to continue,
// while a manual_resolution one stays stopped for a human.
func (r *UncertainReconciler) Sweep(ctx context.Context) (int, error) {
	calls, err := r.store.UncertainToolCalls(ctx, r.batch)
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, call := range calls {
		resolution, result, err := r.resolve(ctx, call)
		if err != nil {
			return resolved, err
		}
		if err := r.store.ReconcileToolCall(ctx, call.Tenant, call.SessionID, call.ResponseID, call.RunID, call.CallID, resolution, result); err != nil {
			return resolved, err
		}
		resolved++
		// A reconciled_* exit lets the run continue; manual_resolution keeps it stopped for a human.
		if resolution != "manual_resolution" {
			if err := r.store.ReenqueueResponseRun(ctx, call.Tenant, call.RunID); err != nil {
				return resolved, err
			}
		}
	}
	return resolved, nil
}

// resolve decides one uncertain call's exit (spec §26.7). Irreversible/interactive NEVER auto-resolve —
// a human must confirm an irreversible effect. Reversible (and idempotent, treated the same) probe the
// destination: applied → reconciled_completed with the probed result; not applied → reconciled_not_applied.
// A tool with no probe surface, or no prober wired, escalates to manual_resolution rather than guessing.
func (r *UncertainReconciler) resolve(ctx context.Context, call coordinator.UncertainToolCall) (resolution string, result []byte, err error) {
	switch toolbroker.ReplayClass(call.ReplayClass) {
	case toolbroker.ClassIrreversible, toolbroker.ClassInteractive:
		return "manual_resolution", nil, nil
	}
	if r.prober == nil {
		return "manual_resolution", nil, nil
	}
	applied, probed, supported, perr := r.prober.Probe(ctx, call)
	if perr != nil {
		return "", nil, perr
	}
	if !supported {
		return "manual_resolution", nil, nil
	}
	if applied {
		if probed == nil {
			probed, _ = json.Marshal(map[string]any{"reconciled": true})
		}
		return "reconciled_completed", probed, nil
	}
	return "reconciled_not_applied", nil, nil
}

// Run sweeps every interval until ctx is cancelled. A sweep error is non-fatal — the next tick retries —
// so a transient database or probe blip never stops the reconcile safety net (reconciler.go's discipline).
func (r *UncertainReconciler) Run(ctx context.Context) error {
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
