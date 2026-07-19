//go:build component

package postgres

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestSupervisorRestartsWorkerAfterTransientAndKeepsDraining proves H2 against real
// PostgreSQL: a background dispatch worker that returns a transient error is not silently
// lost. The supervisor logs it, records a restart, and runs the worker again — and the job
// enqueued before the transient still drains to completed. The restart counter (the value
// doctor surfaces) reflects exactly the one injected transient. Before the supervisor a bare
// `go worker.Run(ctx)` would die on the transient and the job would hang here forever.
func TestSupervisorRestartsWorkerAfterTransientAndKeepsDraining(t *testing.T) {
	store := openHarness(t)
	tenant, _, _ := seedRun(t, store.Pool()) // org+project a durable job can reference
	jobID := newID("job")
	if err := store.Enqueue(context.Background(), tenant, jobID, "response.run"); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}

	// A real worker whose handler completes whatever it claims.
	handler := func(context.Context, coordinator.Claim, []byte) (string, error) { return "supervised-done", nil }
	worker := coordinator.NewWorker(store, coordinator.WorkerConfig{
		Owner: "supervised-worker", Lease: 5 * time.Second, Heartbeat: time.Second, PollInterval: 20 * time.Millisecond,
		Retry: coordinator.RetryPolicy{MaxAttempts: 3, BaseBackoff: 5 * time.Millisecond, MaxBackoff: 20 * time.Millisecond},
	}, handler)

	// Inject one transient claim error: the first supervised run returns a DB-shaped error
	// (exactly what Worker.Run returns when ClaimNext fails transiently); the restart runs the
	// real loop, which drains the queue.
	var runs atomic.Int32
	supervised := func(ctx context.Context) error {
		if runs.Add(1) == 1 {
			return errors.New("transient claim error: connection reset by peer")
		}
		return worker.Run(ctx)
	}

	sup := coordinator.NewSupervisor(func(string, ...any) {}, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sup.Supervise(ctx, "dispatch-worker", supervised)

	// The job completes only if the supervisor restarted the worker after the transient.
	deadline := time.Now().Add(10 * time.Second)
	for {
		snap, err := store.Snapshot(context.Background(), tenant, jobID)
		if err != nil {
			t.Fatalf("Snapshot() error = %v", err)
		}
		if snap.Status == "completed" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("job status = %q after 10s, want completed (supervisor did not restart the worker)", snap.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The counter doctor surfaces reflects exactly the one injected transient.
	if got := sup.Restarts()["dispatch-worker"]; got != 1 {
		t.Fatalf("restart counter = %d, want 1 (the single transient)", got)
	}
}
