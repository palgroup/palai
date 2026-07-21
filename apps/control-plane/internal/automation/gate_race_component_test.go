//go:build component

package automation

import (
	"context"
	"sync"
	"testing"
)

// TestConcurrentSameKeyGateAdmitsExactlyOne pins M3: the concurrency gate is check-then-act, so without a
// lock two concurrent same-key deliveries both see "gate open" and admit TWO parallel runs — violating
// singleton (one active trigger-wide) and queue FIFO. Under a real race (many goroutines, same gate),
// exactly ONE must admit and the rest defer; a DIFFERENT key still runs in parallel.
func TestConcurrentSameKeyGateAdmitsExactlyOne(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)

	// singleton: the gate is trigger-wide, so all concurrent deliveries contend on ONE gate.
	triggerID, _ := seedTrigger(t, store, org, project, "race", TriggerRevisionInput{ConcurrencyPolicy: "singleton"})

	const n = 8
	var wg sync.WaitGroup
	results := make([]string, n)
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release all at once to maximize the race window
			del, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
			results[i], errs[i] = del.State, err
		}(i)
	}
	close(start)
	wg.Wait()

	admitted := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("concurrent delivery %d error = %v", i, errs[i])
		}
		if results[i] == "run_created" {
			admitted++
		}
	}
	if admitted != 1 {
		t.Fatalf("singleton admitted %d runs under a same-key race, want exactly 1 (the rest defer)", admitted)
	}
	// The DB agrees: exactly one non-terminal run for the trigger.
	if got := count(t, pool, `SELECT count(*) FROM runs r JOIN trigger_deliveries d ON d.run_id=r.id WHERE d.trigger_id=$1 AND r.state NOT IN ('completed','failed','canceled','timed_out','budget_exceeded')`, triggerID); got != 1 {
		t.Fatalf("active runs for the singleton trigger = %d, want exactly 1", got)
	}

	// A DIFFERENT trigger's key is unaffected — the lock is per gate scope, not global.
	other, _ := seedTrigger(t, store, org, project, "race-other", TriggerRevisionInput{ConcurrencyPolicy: "queue", CorrelationKeyExpr: `{"select":"k"}`})
	a, _ := store.CreateDelivery(ctx, org, project, principal, other, []byte(`{"k":"x"}`))
	b, _ := store.CreateDelivery(ctx, org, project, principal, other, []byte(`{"k":"y"}`))
	if a.State != "run_created" || b.State != "run_created" {
		t.Fatalf("different keys did not run in parallel: %q, %q", a.State, b.State)
	}
}
