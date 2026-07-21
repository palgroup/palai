//go:build component

package automation

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestQueuePolicyOrdersRunsPerKey pins AUT-004: under the `queue` concurrency policy, deliveries sharing a
// correlation key run FIFO — the first admits, the rest defer, and each next head admits only when the
// prior run terminates (via the supervised reconciler). Deliveries with a DIFFERENT key run in parallel
// (a busy key never blocks a free one).
func TestQueuePolicyOrdersRunsPerKey(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	rec := NewDeliveryReconciler(store, time.Hour, time.Hour, 100, nil)

	triggerID, _ := seedTrigger(t, store, org, project, "queued", TriggerRevisionInput{
		ConcurrencyPolicy: "queue", CorrelationKeyExpr: `{"select":"key"}`,
	})

	// Three same-key events: the first admits, the next two defer.
	d1, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("delivery 1 error = %v", err)
	}
	if d1.State != "run_created" {
		t.Fatalf("delivery 1 state = %q, want run_created", d1.State)
	}
	d2, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("delivery 2 error = %v", err)
	}
	d3, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
	if err != nil {
		t.Fatalf("delivery 3 error = %v", err)
	}
	if d2.State != "deferred" || d3.State != "deferred" {
		t.Fatalf("deliveries 2,3 states = %q,%q; want deferred,deferred", d2.State, d3.State)
	}

	// A DIFFERENT key admits immediately — a busy key does not block a free one.
	other, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"other"}`))
	if err != nil {
		t.Fatalf("other-key delivery error = %v", err)
	}
	if other.State != "run_created" {
		t.Fatalf("other-key delivery state = %q, want run_created (parallel)", other.State)
	}

	// The gate is closed while d1's run is active: a reconciler tick admits nothing.
	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick error = %v", err)
	}
	if got := deliveryState(t, pool, d2.ID); got != "deferred" {
		t.Fatalf("delivery 2 after tick (gate closed) = %q, want deferred", got)
	}

	// Terminate d1's run → the gate opens; a tick admits the FIFO head (d2), not d3.
	completeRun(t, pool, d1.RunID)
	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick error = %v", err)
	}
	if got := deliveryState(t, pool, d2.ID); got != "run_created" {
		t.Fatalf("delivery 2 after gate open = %q, want run_created", got)
	}
	if got := deliveryState(t, pool, d3.ID); got != "deferred" {
		t.Fatalf("delivery 3 = %q, want still deferred (d2 now runs, FIFO)", got)
	}

	// Terminate d2's run → d3 admits.
	d2Run := deliveryRun(t, pool, d2.ID)
	completeRun(t, pool, d2Run)
	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick error = %v", err)
	}
	if got := deliveryState(t, pool, d3.ID); got != "run_created" {
		t.Fatalf("delivery 3 after d2 terminal = %q, want run_created", got)
	}
}

// TestReconcilerRecoversStuckMapped pins the crash-remnant half of AUT-004: a delivery stranded in
// `mapped` past the grace window (a crash between mapping and the concurrency decision) is re-decided by
// the reconciler from its stored mapped_input + hash — it does not sit stuck forever.
func TestReconcilerRecoversStuckMapped(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	// grace 0 so the just-created remnant is immediately eligible.
	rec := NewDeliveryReconciler(store, time.Hour, 0, 100, nil)

	triggerID, _ := seedTrigger(t, store, org, project, "stuck", TriggerRevisionInput{ConcurrencyPolicy: "allow"})

	// A delivery frozen in `mapped` (simulating a crash after the map step) with its input stored.
	deliveryID := insertMappedRemnant(t, pool, org, project, principal, triggerID, store)

	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick error = %v", err)
	}
	if got := deliveryState(t, pool, deliveryID); got != "run_created" {
		t.Fatalf("stuck-mapped delivery after recovery = %q, want run_created", got)
	}
}

// insertMappedRemnant creates a trigger delivery frozen in the `mapped` state with a stored mapped_input
// and an old updated_at, simulating a process crash between mapping and the concurrency decision.
func insertMappedRemnant(t *testing.T, pool *pgxpool.Pool, org, project, principal, triggerID string, store *TriggerStore) string {
	t.Helper()
	rev, ok, err := store.GetActiveRevision(context.Background(), org, project, triggerID)
	if err != nil || !ok {
		t.Fatalf("resolve active revision error = %v ok=%v", err, ok)
	}
	id := randID("tdel")
	mustExec(t, pool,
		`INSERT INTO trigger_deliveries
		 (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id, state, mapped_input, updated_at)
		 VALUES ($1,$2,$3,$4,$5,$6,'mapped','{}'::jsonb, clock_timestamp() - interval '1 minute')`,
		id, org, project, triggerID, rev.ID, principal)
	return id
}

// deliveryState reads a delivery's current state.
func deliveryState(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM trigger_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read delivery state error = %v", err)
	}
	return state
}

// deliveryRun reads a delivery's run id.
func deliveryRun(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var runID string
	if err := pool.QueryRow(context.Background(), `SELECT run_id FROM trigger_deliveries WHERE id=$1`, id).Scan(&runID); err != nil {
		t.Fatalf("read delivery run error = %v", err)
	}
	return runID
}
