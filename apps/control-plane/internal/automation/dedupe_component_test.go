//go:build component

package automation

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestDuplicateDeliveryLinksOriginalSingleAction pins AUT-001: two deliveries carrying the SAME dedupe
// key produce ONE canonical delivery; the second is terminalized `duplicate` and linked to the canonical
// original (original-linkage), so a redelivered source event yields a SINGLE canonical action, not two.
// Under a concurrent race (two goroutines, same key) the partial-unique canonical index still admits
// exactly one canonical — the loser becomes the duplicate.
func TestDuplicateDeliveryLinksOriginalSingleAction(t *testing.T) {
	pool := componentPool(t)
	store := NewTriggerStore(pool)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)

	// A trigger whose dedupe key is the source order id.
	triggerID, _ := seedTrigger(t, store, org, project, "orders", TriggerRevisionInput{
		DedupeKeyExpr: `{"select":"order.id"}`,
	})

	payload := []byte(`{"order":{"id":"o1"}}`)
	first, err := store.CreateDelivery(ctx, org, project, triggerID, payload)
	if err != nil {
		t.Fatalf("first CreateDelivery error = %v", err)
	}
	if first.State != "deduplicated" {
		t.Fatalf("first delivery state = %q, want deduplicated (the canonical)", first.State)
	}

	second, err := store.CreateDelivery(ctx, org, project, triggerID, payload)
	if err != nil {
		t.Fatalf("second CreateDelivery error = %v", err)
	}
	if second.State != "duplicate" {
		t.Fatalf("second delivery state = %q, want duplicate", second.State)
	}
	if second.DuplicateOf != first.ID {
		t.Fatalf("second delivery linked to %q, want the canonical original %q", second.DuplicateOf, first.ID)
	}

	// Exactly one live canonical row for this key.
	canonicals := canonicalCount(t, pool, triggerID, "o1")
	if canonicals != 1 {
		t.Fatalf("live canonical rows for key o1 = %d, want 1", canonicals)
	}

	// Race: two concurrent deliveries with the same fresh key → still exactly one canonical.
	var wg sync.WaitGroup
	racePayload := []byte(`{"order":{"id":"o2"}}`)
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = store.CreateDelivery(ctx, org, project, triggerID, racePayload)
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
}

// canonicalCount reports how many LIVE canonical delivery rows (duplicate_of NULL) hold the given dedupe
// key for a trigger — the invariant the partial-unique index must hold at exactly 1.
func canonicalCount(t *testing.T, pool *pgxpool.Pool, triggerID, key string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM trigger_deliveries WHERE trigger_id = $1 AND dedupe_key = $2 AND duplicate_of IS NULL`,
		triggerID, key).Scan(&n); err != nil {
		t.Fatalf("canonical count error = %v", err)
	}
	return n
}
