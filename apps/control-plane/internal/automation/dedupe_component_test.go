//go:build component

package automation

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/storage"
)

// TestDuplicateDeliveryLinksOriginalSingleAction pins AUT-001: two deliveries carrying the SAME dedupe
// key produce ONE canonical delivery; the second is terminalized `duplicate` and linked to the canonical
// original (original-linkage), so a redelivered source event yields a SINGLE canonical action, not two.
// Under a concurrent race (two goroutines, same key) the partial-unique canonical index still admits
// exactly one canonical — the loser becomes the duplicate.
func TestDuplicateDeliveryLinksOriginalSingleAction(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	// A trigger whose dedupe key is the source order id.
	triggerID, _ := seedTrigger(t, store, org, project, "orders", TriggerRevisionInput{
		DedupeKeyExpr: `{"select":"order.id"}`,
	})

	payload := []byte(`{"order":{"id":"o1"}}`)
	first, err := store.CreateDelivery(ctx, org, project, principal, triggerID, payload)
	if err != nil {
		t.Fatalf("first CreateDelivery error = %v", err)
	}
	// The canonical runs the full pipeline to a born run (AUT-001 "single action" = one run).
	if first.State != "run_created" {
		t.Fatalf("first delivery state = %q, want run_created (the canonical action)", first.State)
	}
	if first.RunID == "" {
		t.Fatal("canonical delivery produced no run")
	}

	second, err := store.CreateDelivery(ctx, org, project, principal, triggerID, payload)
	if err != nil {
		t.Fatalf("second CreateDelivery error = %v", err)
	}
	if second.State != "duplicate" {
		t.Fatalf("second delivery state = %q, want duplicate", second.State)
	}
	if second.DuplicateOf != first.ID {
		t.Fatalf("second delivery linked to %q, want the canonical original %q", second.DuplicateOf, first.ID)
	}
	if second.RunID != "" {
		t.Fatal("duplicate delivery must not produce a run (a redelivered event is a single action)")
	}

	// Exactly one live canonical row and exactly one run for this key.
	if got := canonicalCount(t, pool, triggerID, "o1"); got != 1 {
		t.Fatalf("live canonical rows for key o1 = %d, want 1", got)
	}
	if got := runCount(t, pool, triggerID, "o1"); got != 1 {
		t.Fatalf("runs born for key o1 = %d, want exactly 1", got)
	}

	// Race: two concurrent deliveries with the same fresh key → still exactly one canonical + one run.
	var wg sync.WaitGroup
	racePayload := []byte(`{"order":{"id":"o2"}}`)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.CreateDelivery(ctx, org, project, principal, triggerID, racePayload)
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Fatalf("racing CreateDelivery %d error = %v", i, e)
		}
	}
	if got := canonicalCount(t, pool, triggerID, "o2"); got != 1 {
		t.Fatalf("live canonical rows for key o2 under race = %d, want exactly 1", got)
	}
	if got := runCount(t, pool, triggerID, "o2"); got != 1 {
		t.Fatalf("runs born for key o2 under race = %d, want exactly 1", got)
	}
}

// runCount reports how many runs were born from the canonical delivery for a given dedupe key.
func runCount(t *testing.T, pool *pgxpool.Pool, triggerID, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id = $1 AND dedupe_key = $2 AND run_id <> ''`,
		triggerID, key).Scan(&n); err != nil {
		t.Fatalf("run count error = %v", err)
	}
	return n
}

// canonicalCount reports how many LIVE canonical delivery rows (duplicate_of NULL) hold the given dedupe
// key for a trigger — the invariant the partial-unique index must hold at exactly 1.
func canonicalCount(t *testing.T, pool *pgxpool.Pool, triggerID, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id = $1 AND dedupe_key = $2 AND duplicate_of IS NULL`,
		triggerID, key).Scan(&n); err != nil {
		t.Fatalf("canonical count error = %v", err)
	}
	return n
}
