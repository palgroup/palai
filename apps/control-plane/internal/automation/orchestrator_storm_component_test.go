//go:build component

// The AUT-013 orchestrator-kit leg (E17 T8, spec §35.2 — the single retry owner). A scripted fake
// external orchestrator replays the SAME logical request — the same workflow id + idempotency key —
// under a RETRY STORM. This is the §35.2 invariant made executable, against real PostgreSQL:
//
//   • WITHOUT an idempotency key, the storm MULTIPLIES: N replays of the same body create N distinct
//     runs (a per_event/allow trigger dedupes nothing) — proving the storm is real and would fan out.
//   • WITH the workflow-derived idempotency key, the SAME storm collapses to exactly ONE run — the DB
//     unique index (not app-code) arbitrates the concurrent race, so there is no retry multiplication.
//
// The idempotency key here is derived from the orchestrator's workflow id exactly as the TS kit derives
// it (workflowIdempotencyKey), so the component leg and the SDK helper pin the same seam. The canonical
// run identity is Palai's, minted once; the external workflow id never replaces it. Reconcile-by-key is
// the same replay: a retry after the winner committed returns the winner's run, never a duplicate.
package automation

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"
)

// workflowIdempotencyKey mirrors the TS kit's derivation (sdks/typescript/src/orchestrator.ts): a stable,
// pure function of the external workflow id. Same workflow id ⇒ same key ⇒ the server settles one run.
func workflowIdempotencyKey(workflowID string) string {
	sum := sha256.Sum256([]byte(workflowID))
	return "wf_" + hex.EncodeToString(sum[:])[:32]
}

// TestOrchestratorRetryStormSingleRun is the AUT-013 orchestrator-kit leg: a same-key retry storm yields
// exactly ONE run, while the same storm WITHOUT a key multiplies — the two arms make the single-retry-owner
// invariant executable.
func TestOrchestratorRetryStormSingleRun(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	// A per_event / allow trigger: it dedupes NOTHING on its own, so any collapse is the idempotency key's
	// doing, not a trigger-configured dedupe.
	mapping := []byte(`{"fields":{"input":{"const":"orchestrated work"}}}`)
	noKeyTrigger, _ := seedTrigger(t, store, org, project, "storm-nokey", TriggerRevisionInput{InputMapping: mapping})
	keyedTrigger, _ := seedTrigger(t, store, org, project, "storm-keyed", TriggerRevisionInput{InputMapping: mapping})

	const storm = 16
	body := []byte(`{"order":"o-1"}`)

	// --- RED baseline: WITHOUT idempotency the storm multiplies into N distinct runs ---------------
	noKeyRuns := make(map[string]struct{}, storm)
	for i := 0; i < storm; i++ {
		del, err := store.CreateDelivery(ctx, org, project, principal, noKeyTrigger, body)
		if err != nil {
			t.Fatalf("no-key replay %d CreateDelivery error = %v", i, err)
		}
		if del.RunID == "" {
			t.Fatalf("no-key replay %d did not create a run (state=%s reason=%s)", i, del.State, del.Reason)
		}
		noKeyRuns[del.RunID] = struct{}{}
	}
	if len(noKeyRuns) != storm {
		t.Fatalf("without an idempotency key the storm must multiply: got %d distinct runs, want %d", len(noKeyRuns), storm)
	}
	assertCount(t, pool, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, storm, noKeyTrigger)

	// --- GREEN: WITH the workflow-derived key the SAME storm settles ONE run, race-free ------------
	key := workflowIdempotencyKey("wf-orchestrator-storm")
	var wg sync.WaitGroup
	results := make([]DeliveryResult, storm)
	errs := make([]error, storm)
	for i := 0; i < storm; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = store.CreateDeliveryIdempotent(ctx, org, project, principal, keyedTrigger, key, body)
		}(i)
	}
	wg.Wait()

	// Every replay collapses to the ONE claimed delivery; any run id a replay observes agrees on ONE run.
	// A LOSER of the claim race may see the winner mid-pipeline (state=mapped, no run id yet) — that is the
	// honest "winner shown at its live state", not a second run; the durable rows are the single-run proof.
	deliveryIDs := make(map[string]struct{}, 1)
	runIDs := make(map[string]struct{}, 1)
	for i, err := range errs {
		if err != nil {
			t.Fatalf("keyed replay %d CreateDeliveryIdempotent error = %v", i, err)
		}
		deliveryIDs[results[i].ID] = struct{}{}
		if results[i].RunID != "" {
			runIDs[results[i].RunID] = struct{}{}
		}
	}
	if len(deliveryIDs) != 1 {
		t.Fatalf("the same-key storm must collapse to ONE delivery, got %d distinct", len(deliveryIDs))
	}
	if len(runIDs) != 1 {
		t.Fatalf("the same-key storm must observe ONE run, got %d distinct", len(runIDs))
	}
	// Exactly one durable delivery under the storm — the DB unique index arbitrated the race.
	assertCount(t, pool, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, 1, keyedTrigger)

	// Reconcile-by-key: a post-storm replay (the orchestrator recovering with only its workflow id)
	// re-derives the key and resolves back to the SAME settled run — never a duplicate. By now the winner
	// has driven the pipeline to run_created, so the replay carries the canonical run + response identity.
	reconciled, err := store.CreateDeliveryIdempotent(ctx, org, project, principal, keyedTrigger, key, body)
	if err != nil {
		t.Fatalf("reconcile replay error = %v", err)
	}
	if reconciled.RunID == "" {
		t.Fatalf("post-storm reconcile has no run id (state=%s)", reconciled.State)
	}
	if _, ok := runIDs[reconciled.RunID]; !ok {
		t.Fatalf("reconcile-by-key returned run %q, not the storm's one run", reconciled.RunID)
	}
	assertCount(t, pool, `SELECT count(*) FROM responses WHERE id=$1`, 1, reconciled.ResponseID)
	assertCount(t, pool, `SELECT count(*) FROM trigger_deliveries WHERE trigger_id=$1`, 1, keyedTrigger)
}
