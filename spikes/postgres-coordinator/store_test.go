package coordinator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

const testLeaseDuration = 150 * time.Millisecond

func TestKilledWorkerIsReclaimedWithHigherFence(t *testing.T) {
	store := openTestStore(t)
	for iteration := 0; iteration < testIterations(t); iteration++ {
		jobID := testJobID(t, iteration)
		workerA, workerB := killAndReclaim(t, store, jobID)
		if workerB.Fence <= workerA.Fence {
			t.Fatalf("worker B fence = %d, want > worker A fence %d", workerB.Fence, workerA.Fence)
		}
		snapshot := readSnapshot(t, store, jobID)
		if snapshot.Status != "running" || snapshot.Fence != workerB.Fence || snapshot.LeaseOwner != workerB.Owner {
			t.Fatalf("reclaimed snapshot = %+v, want running worker B", snapshot)
		}
		if snapshot.AttemptCount != 2 {
			t.Fatalf("attempt count = %d, want 2", snapshot.AttemptCount)
		}
	}
}

func TestStaleCompletionCannotWriteResultOrOutbox(t *testing.T) {
	store := openTestStore(t)
	for iteration := 0; iteration < testIterations(t); iteration++ {
		jobID := testJobID(t, iteration)
		workerA, workerB := killAndReclaim(t, store, jobID)
		err := store.Complete(t.Context(), workerA, "stale-result")
		if !errors.Is(err, ErrStaleFence) {
			t.Fatalf("stale Complete() error = %v, want ErrStaleFence", err)
		}
		snapshot := readSnapshot(t, store, jobID)
		if snapshot.Status != "running" || snapshot.Fence != workerB.Fence {
			t.Fatalf("snapshot after stale completion = %+v", snapshot)
		}
		if snapshot.ResultHash != nil || snapshot.OutboxCount != 0 {
			t.Fatalf("stale completion wrote result/outbox: %+v", snapshot)
		}
	}
}

func TestOneAuthoritativeCompletionAndOutbox(t *testing.T) {
	store := openTestStore(t)
	for iteration := 0; iteration < testIterations(t); iteration++ {
		jobID := testJobID(t, iteration)
		_, workerB := killAndReclaim(t, store, jobID)
		resultHashes := []string{
			fmt.Sprintf("authoritative-a-%d", iteration),
			fmt.Sprintf("authoritative-b-%d", iteration),
		}
		type completionResult struct {
			resultHash string
			err        error
		}
		results := make(chan completionResult, len(resultHashes))
		start := make(chan struct{})
		var workers sync.WaitGroup
		for _, resultHash := range resultHashes {
			workers.Add(1)
			go func() {
				defer workers.Done()
				<-start
				results <- completionResult{resultHash: resultHash, err: store.Complete(t.Context(), workerB, resultHash)}
			}()
		}
		close(start)
		workers.Wait()
		close(results)
		successes := 0
		stale := 0
		winningHash := ""
		for result := range results {
			switch {
			case result.err == nil:
				successes++
				winningHash = result.resultHash
			case errors.Is(result.err, ErrStaleFence):
				stale++
			default:
				t.Fatalf("concurrent Complete() error = %v", result.err)
			}
		}
		if successes != 1 || stale != 1 {
			t.Fatalf("completion outcomes: success=%d stale=%d, want 1/1", successes, stale)
		}
		if err := store.Complete(t.Context(), workerB, "duplicate"); !errors.Is(err, ErrStaleFence) {
			t.Fatalf("duplicate Complete() error = %v, want ErrStaleFence", err)
		}
		snapshot := readSnapshot(t, store, jobID)
		if snapshot.Status != "completed" || snapshot.ResultHash == nil || *snapshot.ResultHash != winningHash {
			t.Fatalf("completed snapshot = %+v", snapshot)
		}
		if snapshot.OutboxCount != 1 || snapshot.OutboxFence == nil || *snapshot.OutboxFence != workerB.Fence {
			t.Fatalf("authoritative outbox snapshot = %+v", snapshot)
		}
	}
}

func TestTransactionKillLeavesClaimRecoverable(t *testing.T) {
	store := openTestStore(t)
	for iteration := 0; iteration < testIterations(t); iteration++ {
		jobID := testJobID(t, iteration)
		if err := store.Enqueue(t.Context(), jobID); err != nil {
			t.Fatalf("Enqueue() error = %v", err)
		}
		workerA := claimOnceInProcess(t, jobID, "transaction-a", testLeaseDuration)
		held := startWorkerProcess(t, workerRequest{
			Action:     "complete-hold",
			JobID:      workerA.JobID,
			Owner:      workerA.Owner,
			Fence:      workerA.Fence,
			ResultHash: "uncommitted-result",
		})
		receipt := held.readReceipt(t)
		if receipt.Kind != "completion-staged" || receipt.Fence != workerA.Fence {
			t.Fatalf("held completion receipt = %+v", receipt)
		}
		held.kill(t)

		rolledBack := readSnapshot(t, store, jobID)
		if rolledBack.Status != "running" || rolledBack.Fence != workerA.Fence {
			t.Fatalf("snapshot after transaction kill = %+v", rolledBack)
		}
		if rolledBack.ResultHash != nil || rolledBack.OutboxCount != 0 {
			t.Fatalf("killed transaction committed partial state: %+v", rolledBack)
		}

		waitForLeaseExpiry(t, store, jobID)
		workerB := claimOnceInProcess(t, jobID, "transaction-b", testLeaseDuration)
		if workerB.Fence <= workerA.Fence {
			t.Fatalf("worker B fence = %d, want > %d", workerB.Fence, workerA.Fence)
		}
		if err := store.Complete(t.Context(), workerB, "recovered-result"); err != nil {
			t.Fatalf("recovered Complete() error = %v", err)
		}
		recovered := readSnapshot(t, store, jobID)
		if recovered.Status != "completed" || recovered.OutboxCount != 1 {
			t.Fatalf("recovered snapshot = %+v", recovered)
		}
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("PALAI_SPIKE_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_SPIKE_POSTGRES_URL is required")
	}
	store, err := NewStore(t.Context(), url)
	if err != nil {
		t.Fatalf("NewStore() error = %v", err)
	}
	t.Cleanup(store.Close)
	if err := store.ApplySchema(t.Context()); err != nil {
		t.Fatalf("ApplySchema() error = %v", err)
	}
	return store
}

func killAndReclaim(t *testing.T, store *Store, jobID string) (Claim, Claim) {
	t.Helper()
	if err := store.Enqueue(t.Context(), jobID); err != nil {
		t.Fatalf("Enqueue() error = %v", err)
	}
	held := startWorkerProcess(t, workerRequest{
		Action: "claim-hold",
		JobID:  jobID,
		Owner:  "worker-a",
		Lease:  testLeaseDuration,
	})
	workerA := held.readReceipt(t).Claim
	if workerA.JobID != jobID || workerA.Fence < 1 {
		t.Fatalf("worker A claim = %+v", workerA)
	}
	held.kill(t)
	waitForLeaseExpiry(t, store, jobID)
	workerB := claimOnceInProcess(t, jobID, "worker-b", testLeaseDuration)
	return workerA, workerB
}

func claimOnceInProcess(t *testing.T, jobID, owner string, lease time.Duration) Claim {
	t.Helper()
	process := startWorkerProcess(t, workerRequest{
		Action: "claim-once",
		JobID:  jobID,
		Owner:  owner,
		Lease:  lease,
	})
	receipt := process.readReceipt(t)
	process.waitSuccess(t)
	if receipt.Kind != "claim" {
		t.Fatalf("claim receipt = %+v", receipt)
	}
	return receipt.Claim
}

func waitForLeaseExpiry(t *testing.T, store *Store, jobID string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	for {
		expired, err := store.LeaseExpired(ctx, jobID)
		if err != nil {
			t.Fatalf("LeaseExpired() error = %v", err)
		}
		if expired {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("lease did not expire: %v", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func readSnapshot(t *testing.T, store *Store, jobID string) JobSnapshot {
	t.Helper()
	snapshot, err := store.Snapshot(t.Context(), jobID)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	return snapshot
}

func testIterations(t *testing.T) int {
	t.Helper()
	value := os.Getenv("PALAI_SPIKE_ITERATIONS")
	if value == "" {
		return 1
	}
	var iterations int
	if _, err := fmt.Sscanf(value, "%d", &iterations); err != nil || iterations < 1 || iterations > 100 {
		t.Fatalf("PALAI_SPIKE_ITERATIONS = %q, want 1..100", value)
	}
	return iterations
}

func testJobID(t *testing.T, iteration int) string {
	t.Helper()
	name := strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-"))
	return fmt.Sprintf("%s-%d-%d", name, os.Getpid(), iteration)
}
