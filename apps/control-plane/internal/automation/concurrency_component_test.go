//go:build component

package automation

import (
	"context"
	"testing"
	"time"
)

// TestConcurrencyPoliciesDocumentedOutcomes pins AUT-005: the remaining concurrency policies produce
// their documented outcomes. drop_if_running SKIPS a new event while the key runs (a policy skip, its own
// terminal — honest naming, not a rejection); singleton keeps a single active run trigger-wide and defers
// the rest; coalesce collapses a burst of deferred events into one survivor (the latest) and skips the
// subsumed rows linked to it; replace cancels the active run and admits the new event in its place.
func TestConcurrencyPoliciesDocumentedOutcomes(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	rec := NewDeliveryReconciler(store, time.Hour, time.Hour, 100, nil)

	t.Run("drop_if_running skips while the key runs", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "drop", TriggerRevisionInput{
			ConcurrencyPolicy: "drop_if_running", CorrelationKeyExpr: `{"select":"key"}`,
		})
		if _, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`)); err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		dropped, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if dropped.State != "skipped" {
			t.Fatalf("drop_if_running second delivery state = %q, want skipped", dropped.State)
		}
		if dropped.Reason == "" {
			t.Fatal("a skipped delivery must record a reason")
		}
		if dropped.RunID != "" {
			t.Fatal("a dropped delivery must not create a run")
		}
	})

	t.Run("singleton keeps one active trigger-wide", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "singleton", TriggerRevisionInput{ConcurrencyPolicy: "singleton"})
		first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if second.State != "deferred" {
			t.Fatalf("singleton second delivery state = %q, want deferred", second.State)
		}
		// Free the trigger → the reconciler admits the deferred one.
		completeRun(t, pool, first.RunID)
		if err := rec.Tick(ctx); err != nil {
			t.Fatalf("tick error = %v", err)
		}
		if got := deliveryState(t, pool, second.ID); got != "run_created" {
			t.Fatalf("singleton deferred delivery after gate open = %q, want run_created", got)
		}
	})

	t.Run("coalesce collapses a burst into the latest survivor", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "coalesce", TriggerRevisionInput{
			ConcurrencyPolicy: "coalesce", CorrelationKeyExpr: `{"select":"key"}`,
		})
		first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		// Two more events burst in while the first runs → both defer.
		mid, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("mid delivery error = %v", err)
		}
		last, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("last delivery error = %v", err)
		}
		if mid.State != "deferred" || last.State != "deferred" {
			t.Fatalf("coalesce burst states = %q,%q; want deferred,deferred", mid.State, last.State)
		}
		// Free the key → the reconciler admits the survivor (latest = last) and skips the subsumed mid.
		completeRun(t, pool, first.RunID)
		if err := rec.Tick(ctx); err != nil {
			t.Fatalf("tick error = %v", err)
		}
		if got := deliveryState(t, pool, last.ID); got != "run_created" {
			t.Fatalf("coalesce survivor state = %q, want run_created", got)
		}
		if got := deliveryState(t, pool, mid.ID); got != "skipped" {
			t.Fatalf("coalesce subsumed state = %q, want skipped", got)
		}
		// The subsumed row is LINKED to the survivor (recorded, not lost).
		var link *string
		if err := pool.QueryRow(ctx, `SELECT duplicate_of FROM trigger_deliveries WHERE id=$1`, mid.ID).Scan(&link); err != nil {
			t.Fatalf("read coalesce link error = %v", err)
		}
		if link == nil || *link != last.ID {
			t.Fatalf("coalesce subsumed link = %v, want the survivor %q", link, last.ID)
		}
	})

	t.Run("replace cancels the active run and admits the new event", func(t *testing.T) {
		triggerID, _ := seedTrigger(t, store, org, project, "replace", TriggerRevisionInput{
			ConcurrencyPolicy: "replace", CorrelationKeyExpr: `{"select":"key"}`,
		})
		first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("first delivery error = %v", err)
		}
		second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"key":"k"}`))
		if err != nil {
			t.Fatalf("second delivery error = %v", err)
		}
		if second.State != "run_created" {
			t.Fatalf("replace second delivery state = %q, want run_created", second.State)
		}
		if second.RunID == first.RunID {
			t.Fatal("replace must admit a NEW run, not reuse the replaced one")
		}
		// The first (replaced) run is canceled.
		var firstState string
		if err := pool.QueryRow(ctx, `SELECT state FROM runs WHERE id=$1`, first.RunID).Scan(&firstState); err != nil {
			t.Fatalf("read replaced run state error = %v", err)
		}
		if firstState != "canceled" {
			t.Fatalf("replaced run state = %q, want canceled", firstState)
		}
	})
}
