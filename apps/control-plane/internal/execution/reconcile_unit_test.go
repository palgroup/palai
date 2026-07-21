package execution

import (
	"context"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// fakeReconcileStore records the exits the reconcile loop drives and which runs it re-enqueues, so the
// three-exit policy is provable without a database.
type fakeReconcileStore struct {
	uncertain  []coordinator.UncertainToolCall
	resolved   map[string]string
	results    map[string]string
	reenqueued []string
}

func newFakeReconcileStore(calls ...coordinator.UncertainToolCall) *fakeReconcileStore {
	return &fakeReconcileStore{uncertain: calls, resolved: map[string]string{}, results: map[string]string{}}
}

func (f *fakeReconcileStore) UncertainToolCalls(context.Context, int) ([]coordinator.UncertainToolCall, error) {
	return f.uncertain, nil
}
func (f *fakeReconcileStore) ReconcileToolCall(_ context.Context, _ coordinator.Tenant, _, _, _, callID, resolution string, result []byte) error {
	f.resolved[callID] = resolution
	f.results[callID] = string(result)
	return nil
}
func (f *fakeReconcileStore) ReenqueueResponseRun(_ context.Context, _ coordinator.Tenant, runID string) error {
	f.reenqueued = append(f.reenqueued, runID)
	return nil
}

// fakeProber answers the destination probe deterministically.
type fakeProber struct {
	applied   bool
	result    []byte
	supported bool
}

func (p fakeProber) Probe(context.Context, coordinator.UncertainToolCall) (bool, []byte, bool, error) {
	return p.applied, p.result, p.supported, nil
}

// TestReversibleReconcilesThenCompensates proves TOL-004 (spec §26.7): a reversible uncertain call is
// resolved by PROBING the destination — applied → reconciled_completed with the probed result (the run
// continues), not applied → reconciled_not_applied (a typed not-applied result). Either way the run is
// re-enqueued so a fresh attempt proceeds on the resolved row.
func TestReversibleReconcilesThenCompensates(t *testing.T) {
	ctx := context.Background()
	call := coordinator.UncertainToolCall{CallID: "tc_rev", RunID: "run_1", ReplayClass: "reversible"}

	// Applied at the destination → reconciled_completed with the probed result.
	appliedStore := newFakeReconcileStore(call)
	rec := NewUncertainReconciler(appliedStore, fakeProber{applied: true, result: []byte(`{"remote":"ok"}`), supported: true}, time.Second, 10)
	if n, err := rec.Sweep(ctx); err != nil || n != 1 {
		t.Fatalf("Sweep() = (%d, %v), want (1, nil)", n, err)
	}
	if appliedStore.resolved["tc_rev"] != "reconciled_completed" {
		t.Fatalf("applied reversible resolved to %q, want reconciled_completed", appliedStore.resolved["tc_rev"])
	}
	if appliedStore.results["tc_rev"] != `{"remote":"ok"}` {
		t.Fatalf("reconciled result = %q, want the probed result", appliedStore.results["tc_rev"])
	}
	if len(appliedStore.reenqueued) != 1 || appliedStore.reenqueued[0] != "run_1" {
		t.Fatalf("re-enqueued = %v, want [run_1] (the run continues on the resolved row)", appliedStore.reenqueued)
	}

	// NOT applied at the destination → reconciled_not_applied (compensate), still re-enqueued.
	notStore := newFakeReconcileStore(call)
	recNot := NewUncertainReconciler(notStore, fakeProber{applied: false, supported: true}, time.Second, 10)
	if _, err := recNot.Sweep(ctx); err != nil {
		t.Fatalf("Sweep(not applied) error = %v", err)
	}
	if notStore.resolved["tc_rev"] != "reconciled_not_applied" {
		t.Fatalf("not-applied reversible resolved to %q, want reconciled_not_applied", notStore.resolved["tc_rev"])
	}
	if len(notStore.reenqueued) != 1 {
		t.Fatalf("re-enqueued = %v, want the run continued", notStore.reenqueued)
	}
}

// TestIrreversibleEscalatesToManualResolutionNeverReenqueues proves TOL-003's reconcile policy (spec
// §26.7): an irreversible uncertain call is NEVER auto-resolved — the destination is not even probed — it
// escalates to manual_resolution, and the run is NOT re-enqueued (it stays stopped until a human decides).
func TestIrreversibleEscalatesToManualResolutionNeverReenqueues(t *testing.T) {
	ctx := context.Background()
	call := coordinator.UncertainToolCall{CallID: "tc_irr", RunID: "run_2", ReplayClass: "irreversible"}
	store := newFakeReconcileStore(call)
	// A prober that would claim "applied" — it must NOT be consulted for an irreversible call.
	rec := NewUncertainReconciler(store, fakeProber{applied: true, supported: true}, time.Second, 10)

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if store.resolved["tc_irr"] != "manual_resolution" {
		t.Fatalf("irreversible resolved to %q, want manual_resolution (never auto-replays)", store.resolved["tc_irr"])
	}
	if len(store.reenqueued) != 0 {
		t.Fatalf("re-enqueued %v, want none (an irreversible uncertain run stays stopped for a human)", store.reenqueued)
	}
}

// TestNoProberEscalatesToManualResolution proves the honest fallback: a reversible call with no probe
// surface (or no prober wired) escalates to manual_resolution rather than guessing the effect landed.
func TestNoProberEscalatesToManualResolution(t *testing.T) {
	ctx := context.Background()
	call := coordinator.UncertainToolCall{CallID: "tc_rev", RunID: "run_3", ReplayClass: "reversible"}
	store := newFakeReconcileStore(call)
	rec := NewUncertainReconciler(store, nil, time.Second, 10) // no prober

	if _, err := rec.Sweep(ctx); err != nil {
		t.Fatalf("Sweep() error = %v", err)
	}
	if store.resolved["tc_rev"] != "manual_resolution" {
		t.Fatalf("reversible with no prober resolved to %q, want manual_resolution (never guesses)", store.resolved["tc_rev"])
	}
}
