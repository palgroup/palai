//go:build fault

package coordinator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestKilledWorkerIsReclaimedWithHigherFence runs the durable worker loop, kills the
// holder mid-lease, and proves a second worker reclaims the same logical job at a
// strictly higher fence while the killed worker's completion callback is rejected as
// a lease_conflict. This is the productionized postgres-coordinator spike guarantee
// against the real tenant-scoped schema.
func TestKilledWorkerIsReclaimedWithHigherFence(t *testing.T) {
	store := openStore(t)
	tenant := seedTenant(t, store.Pool())
	jobID := enqueueJob(t, store, tenant)
	lease := faultLease(t)

	// Worker A runs the real claim/heartbeat loop; its handler blocks until the
	// worker is killed, so the job is claimed but never completed.
	claimCh := make(chan coordinator.Claim, 1)
	handler := func(hctx context.Context, claim coordinator.Claim, _ []byte) (string, error) {
		claimCh <- claim
		<-hctx.Done() // the kill: the worker stops heartbeating without completing
		return "", hctx.Err()
	}
	workerA := coordinator.NewWorker(store, coordinator.WorkerConfig{
		Owner:        "worker-a",
		Lease:        lease,
		Heartbeat:    lease / 3,
		PollInterval: lease / 10,
		Retry:        coordinator.RetryPolicy{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond},
	}, handler)

	ctxA, killA := context.WithCancel(context.Background())
	runA := make(chan error, 1)
	go func() { runA <- workerA.Run(ctxA) }()

	claimA := <-claimCh
	if claimA.JobID != jobID || claimA.Fence < 1 {
		t.Fatalf("worker A claim = %+v, want job %s fence >= 1", claimA, jobID)
	}

	killA()
	if err := <-runA; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatalf("worker A Run() error = %v, want clean shutdown", err)
	}

	// The killed worker left the job running under its lease; it must lapse by
	// database time before anyone else can take over.
	waitLeaseExpiry(t, store, tenant, jobID)

	claimB, _, err := store.ClaimNext(context.Background(), "worker-b", lease)
	if err != nil {
		t.Fatalf("worker B ClaimNext() error = %v", err)
	}
	if claimB.JobID != jobID {
		t.Fatalf("worker B claimed %s, want the same logical job %s", claimB.JobID, jobID)
	}
	if claimB.Fence <= claimA.Fence {
		t.Fatalf("worker B fence = %d, want > worker A fence %d", claimB.Fence, claimA.Fence)
	}
	if claimB.AttemptCount != claimA.AttemptCount+1 {
		t.Fatalf("worker B attempt count = %d, want %d", claimB.AttemptCount, claimA.AttemptCount+1)
	}

	// The killed worker's completion callback is stale: it owns no live fence, so it
	// writes nothing and is reported as a lease_conflict (the API-level 409).
	if err := store.Complete(context.Background(), claimA, "stale-result"); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("killed worker Complete() error = %v, want ErrStaleFence", err)
	}
	if coordinator.ErrStaleFence.Error() != "lease_conflict" {
		t.Fatalf("ErrStaleFence code = %q, want lease_conflict", coordinator.ErrStaleFence.Error())
	}

	// Only the live fence writes the authoritative result and its single outbox row.
	if err := store.Complete(context.Background(), claimB, "recovered-result"); err != nil {
		t.Fatalf("authoritative Complete() error = %v", err)
	}
	snap := readSnapshot(t, store, tenant, jobID)
	if snap.Status != "completed" || snap.ResultHash == nil || *snap.ResultHash != "recovered-result" {
		t.Fatalf("reclaimed snapshot = %+v, want completed/recovered-result", snap)
	}
}
