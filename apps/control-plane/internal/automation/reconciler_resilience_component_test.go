//go:build component

package automation

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBoundedKeyReuseActiveConflictDefers pins M2 (sync path): a bounded_key_reuse delivery whose chained
// session already holds an active root run must DEFER (the queue signal), not error out to a 500. The
// reconciler admits it once the prior run terminates.
func TestBoundedKeyReuseActiveConflictDefers(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	rec := NewDeliveryReconciler(store, time.Hour, time.Hour, 100, nil)

	triggerID, _ := seedTrigger(t, store, org, project, "chain", TriggerRevisionInput{
		CorrelationMode: "bounded_key_reuse", CorrelationKeyExpr: `{"select":"corr"}`,
	})
	first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c1"}`))
	if err != nil || first.State != "run_created" {
		t.Fatalf("first delivery = %+v, err = %v; want run_created", first, err)
	}
	// The prior session's run is still active → a same-key chain must defer, not 500.
	second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"corr":"c1"}`))
	if err != nil {
		t.Fatalf("chained delivery onto an active session errored instead of deferring: %v", err)
	}
	if second.State != "deferred" {
		t.Fatalf("chained delivery onto an active session state = %q, want deferred", second.State)
	}
	// When the prior run terminates, the reconciler admits the deferred chain onto the SAME session.
	completeRun(t, pool, first.RunID)
	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick error = %v", err)
	}
	if got := deliveryState(t, pool, second.ID); got != "run_created" {
		t.Fatalf("deferred chain after gate open = %q, want run_created", got)
	}
}

// TestReconcilerSweepSkipsPoisonRow pins M2 (sweep path): one poison deferred delivery (its resume errors)
// must not wedge the whole sweep — a healthy deferred delivery in a different group still advances in the
// same tick.
func TestReconcilerSweepSkipsPoisonRow(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	rec := NewDeliveryReconciler(store, time.Hour, time.Hour, 100, nil)

	triggerID, revID := seedTrigger(t, store, org, project, "poison", TriggerRevisionInput{ConcurrencyPolicy: "queue"})

	// A poison deferred delivery: a NON-EXISTENT principal → admission FKs principal_id → resume errors.
	poison := insertDeferredDelivery(t, pool, org, project, "prin_ghost_does_not_exist", triggerID, revID, "poisonhash", `{}`)
	// A healthy deferred delivery in a DIFFERENT key group, gate open.
	healthy := insertDeferredDelivery(t, pool, org, project, principal, triggerID, revID, "healthyhash", `{}`)

	if err := rec.Tick(ctx); err != nil {
		t.Fatalf("tick returned an error (a poison row wedged the sweep): %v", err)
	}
	if got := deliveryState(t, pool, healthy); got != "run_created" {
		t.Fatalf("healthy deferred delivery after tick = %q, want run_created (poison must not block it)", got)
	}
	if got := deliveryState(t, pool, poison); got != "deferred" {
		t.Fatalf("poison delivery = %q, want still deferred (skipped, not advanced)", got)
	}
}

// insertDeferredDelivery inserts a delivery frozen in `deferred` with a stored mapped_input + hash,
// simulating a delivery the concurrency gate parked for the reconciler.
func insertDeferredDelivery(t *testing.T, pool *pgxpool.Pool, org, project, principal, triggerID, revisionID, hash, mappedInput string) string {
	t.Helper()
	id := randID("tdel")
	mustExec(t, pool,
		`INSERT INTO trigger_deliveries
		 (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id, state, mapped_input, correlation_key_hash)
		 VALUES ($1,$2,$3,$4,$5,$6,'deferred',$7::jsonb,$8)`,
		id, org, project, triggerID, revisionID, principal, mappedInput, hash)
	return id
}
