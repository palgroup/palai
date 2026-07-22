//go:build component

// Real-PostgreSQL component tests for AUT-013 per-key delivery idempotency (spec §20.9, §20.2.2, E11
// Task 6). An external orchestrator retries a delivery POST under a stable Idempotency-Key; the same key
// + same body must resolve to ONE delivery / run / callback across every retry, and a race between two
// concurrent retries must still collapse to one durable row (the idempotency_records DB unique index is
// the arbiter, not an app-code check-then-set). A key reused with a DIFFERENT body is a typed conflict.
package automation

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// assertCount runs a scalar count query and fails unless it equals want.
func assertCount(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, query string, want int, args ...any) {
	t.Helper()
	var got int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()), query, args...).Scan(&got); err != nil {
		t.Fatalf("count query %q error = %v", query, err)
	}
	if got != want {
		t.Fatalf("count %q = %d, want %d", query, got, want)
	}
}

// TestOrchestratorRetrySameIdempotencyKeySingleEverything pins AUT-013: three POSTs of the SAME
// Idempotency-Key + body yield ONE delivery and ONE run; a two-goroutine race under the same key collapses
// to a single durable delivery row (the DB unique index arbitrates, race-free).
func TestOrchestratorRetrySameIdempotencyKeySingleEverything(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID, _ := seedTrigger(t, store, org, project, "orders", TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"const":"do the work"}}}`),
	})

	payload := []byte(`{"order":"o1"}`)
	key := "orchestrator-retry-1"

	var first DeliveryResult
	for i := 0; i < 3; i++ {
		del, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, key, payload)
		if err != nil {
			t.Fatalf("retry %d CreateDeliveryIdempotent error = %v", i, err)
		}
		if i == 0 {
			first = del
			continue
		}
		if del.ID != first.ID {
			t.Fatalf("retry %d returned delivery %q, want the same %q", i, del.ID, first.ID)
		}
		if del.RunID != first.RunID {
			t.Fatalf("retry %d returned run %q, want the same %q", i, del.RunID, first.RunID)
		}
	}

	// Exactly one delivery and one run across the three retries.
	assertCount(t, pool, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, 1, triggerID)
	assertCount(t, pool, `SELECT count(*) FROM responses WHERE id=$1`, 1, first.ResponseID)

	// Race variant: two concurrent retries under a NEW key collapse to one delivery row.
	raceKey := "orchestrator-race"
	racePayload := []byte(`{"order":"race"}`)
	var wg sync.WaitGroup
	results := make([]DeliveryResult, 2)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, raceKey, racePayload)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("race goroutine %d error = %v", i, err)
		}
	}
	if results[0].ID != results[1].ID {
		t.Fatalf("race produced two deliveries %q and %q, want one", results[0].ID, results[1].ID)
	}
	assertCount(t, pool, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1 AND id=$2`, 1, triggerID, results[0].ID)
}

// TestSameIdempotencyKeyDifferentBodyConflict409 pins the AUT-013 conflict half: the SAME key reused with
// a DIFFERENT body is a typed mismatch (a body change under a stable key is a client error, never a silent
// second action).
func TestSameIdempotencyKeyDifferentBodyConflict409(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID, _ := seedTrigger(t, store, org, project, "conflict", TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"const":"x"}}}`),
	})

	key := "same-key-different-body"
	if _, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, key, []byte(`{"a":1}`)); err != nil {
		t.Fatalf("first CreateDeliveryIdempotent error = %v", err)
	}
	if _, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, key, []byte(`{"a":2}`)); !errors.Is(err, ErrIdempotencyMismatch) {
		t.Fatalf("second (different body) error = %v, want ErrIdempotencyMismatch", err)
	}
}

// TestRetryAfterTriggerDisabledReplaysWinner pins replay honesty: once a delivery is recorded under a key,
// a retry replays the winner's real projection even if the trigger was DISABLED in between — an accepted
// delivery is idempotent regardless of later config, so the retry must NOT return ErrTriggerDisabled.
func TestRetryAfterTriggerDisabledReplaysWinner(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID, _ := seedTrigger(t, store, org, project, "disable-replay", TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"const":"x"}}}`),
	})

	payload := []byte(`{"o":1}`)
	key := "retry-after-disable"
	first, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, key, payload)
	if err != nil {
		t.Fatalf("first CreateDeliveryIdempotent error = %v", err)
	}
	if first.State != "run_created" {
		t.Fatalf("first delivery state = %q, want run_created", first.State)
	}

	// The trigger is disabled AFTER the delivery was accepted.
	mustExec(t, pool, `UPDATE triggers SET enabled=false WHERE id=$1`, triggerID)

	// A retry under the same key+body replays the winner's projection, NOT ErrTriggerDisabled.
	retry, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, key, payload)
	if err != nil {
		t.Fatalf("retry after disable error = %v, want the recorded winner (not ErrTriggerDisabled)", err)
	}
	if retry.ID != first.ID || retry.State != "run_created" {
		t.Fatalf("retry replayed %+v, want the winner %s run_created", retry, first.ID)
	}

	// A genuinely-NEW delivery on the disabled trigger still errors (the reorder didn't weaken the gate).
	if _, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, "brand-new-key", payload); !errors.Is(err, ErrTriggerDisabled) {
		t.Fatalf("new delivery on disabled trigger error = %v, want ErrTriggerDisabled", err)
	}
}

// TestOrchestratorRetryDifferentKeySameDedupeSingleAction pins that AUT-013 (per-key) and AUT-001 (source
// dedupe) compose: two DIFFERENT idempotency keys create two delivery rows, but the trigger's dedupe key
// collapses the second to a duplicate linked to the original — ONE run in total.
func TestOrchestratorRetryDifferentKeySameDedupeSingleAction(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	triggerID, _ := seedTrigger(t, store, org, project, "dedupe", TriggerRevisionInput{
		InputMapping:  []byte(`{"fields":{"input":{"const":"x"}}}`),
		DedupeKeyExpr: `{"select":"order.id"}`,
	})

	payload := []byte(`{"order":{"id":"o-shared"}}`)
	first, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, "key-A", payload)
	if err != nil {
		t.Fatalf("first CreateDeliveryIdempotent error = %v", err)
	}
	if first.State != "run_created" || first.RunID == "" {
		t.Fatalf("first delivery = %+v, want run_created with a run", first)
	}
	second, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, triggerID, "key-B", payload)
	if err != nil {
		t.Fatalf("second CreateDeliveryIdempotent error = %v", err)
	}
	if second.State != "duplicate" {
		t.Fatalf("second delivery state = %q, want duplicate (same dedupe key, different idempotency key)", second.State)
	}
	if second.DuplicateOf != first.ID {
		t.Fatalf("second linked to %q, want the original %q", second.DuplicateOf, first.ID)
	}
	if second.RunID != "" {
		t.Fatal("the deduped second delivery bore a run; it must bear none")
	}
}
