//go:build fault

package coordinator

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestStaleFenceCallbacksCannotMutateJob claims, reclaims after expiry, then proves
// every callback from the superseded holder — complete, heartbeat and fail — is
// rejected as a lease_conflict and mutates nothing. Only the live fence writes the
// authoritative result and its single outbox row (spec §53.5, §24.5).
func TestStaleFenceCallbacksCannotMutateJob(t *testing.T) {
	store := openStore(t)
	tenant := seedTenant(t, store.Pool())
	jobID := enqueueJob(t, store, tenant)
	lease := faultLease(t)
	ctx := context.Background()

	stale, err := store.Claim(ctx, tenant, jobID, "worker-a", lease)
	if err != nil {
		t.Fatalf("first Claim() error = %v", err)
	}
	waitLeaseExpiry(t, store, tenant, jobID)
	live, _, err := store.ClaimNext(ctx, "worker-b", lease)
	if err != nil {
		t.Fatalf("reclaim ClaimNext() error = %v", err)
	}
	if live.Fence <= stale.Fence {
		t.Fatalf("reclaim fence = %d, want > %d", live.Fence, stale.Fence)
	}

	policy := coordinator.RetryPolicy{MaxAttempts: 5, BaseBackoff: time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	if err := store.Complete(ctx, stale, "stale-result"); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("stale Complete() error = %v, want ErrStaleFence", err)
	}
	if _, err := store.Heartbeat(ctx, stale, lease); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("stale Heartbeat() error = %v, want ErrStaleFence", err)
	}
	if _, err := store.Fail(ctx, stale, policy); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("stale Fail() error = %v, want ErrStaleFence", err)
	}

	// None of the stale callbacks moved the job off the live holder.
	if snap := readSnapshot(t, store, tenant, jobID); snap.Status != "running" || snap.Fence != live.Fence || snap.ResultHash != nil {
		t.Fatalf("snapshot after stale callbacks = %+v, want running on live fence", snap)
	}

	// The live fence completes authoritatively: one result, exactly one outbox row.
	if err := store.Complete(ctx, live, "final-result"); err != nil {
		t.Fatalf("authoritative Complete() error = %v", err)
	}
	if snap := readSnapshot(t, store, tenant, jobID); snap.Status != "completed" || snap.ResultHash == nil || *snap.ResultHash != "final-result" {
		t.Fatalf("completed snapshot = %+v, want completed/final-result", snap)
	}
	dedupe := "job:" + jobID + ":fence:" + strconv.FormatInt(live.Fence, 10) + ":completed"
	var outbox int
	if err := store.Pool().QueryRow(ctx, `SELECT count(*) FROM outbox WHERE dedupe_key = $1`, dedupe).Scan(&outbox); err != nil {
		t.Fatalf("count completion outbox error = %v", err)
	}
	if outbox != 1 {
		t.Fatalf("authoritative completion outbox rows = %d, want 1", outbox)
	}
}

// TestHeartbeatRenewsLeaseByDatabaseTime proves a heartbeat extends a live lease by
// the database clock, and that once the job is reclaimed the previous holder's
// heartbeat is rejected without extending anything.
func TestHeartbeatRenewsLeaseByDatabaseTime(t *testing.T) {
	store := openStore(t)
	tenant := seedTenant(t, store.Pool())
	jobID := enqueueJob(t, store, tenant)
	lease := faultLease(t)
	ctx := context.Background()

	claim, err := store.Claim(ctx, tenant, jobID, "worker-a", lease)
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	renewed, err := store.Heartbeat(ctx, claim, lease)
	if err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if !renewed.After(claim.LeaseExpiresAt) {
		t.Fatalf("heartbeat expiry = %v, want after original %v", renewed, claim.LeaseExpiresAt)
	}

	waitLeaseExpiry(t, store, tenant, jobID)
	if _, _, err := store.ClaimNext(ctx, "worker-b", lease); err != nil {
		t.Fatalf("reclaim ClaimNext() error = %v", err)
	}
	if _, err := store.Heartbeat(ctx, claim, lease); !errors.Is(err, coordinator.ErrStaleFence) {
		t.Fatalf("stale Heartbeat() after reclaim error = %v, want ErrStaleFence", err)
	}
}

// TestExhaustedAttemptsDeadLetter drives the claim/fail retry loop and proves a job
// is requeued with a persisted backoff deadline until its attempts are exhausted,
// then dead-lettered. The attempt count is canonical in the row; the worker never
// hidden-retries.
func TestExhaustedAttemptsDeadLetter(t *testing.T) {
	store := openStore(t)
	tenant := seedTenant(t, store.Pool())
	jobID := enqueueJob(t, store, tenant)
	lease := faultLease(t)
	ctx := context.Background()

	const maxAttempts = 3
	policy := coordinator.RetryPolicy{MaxAttempts: maxAttempts, BaseBackoff: time.Millisecond, MaxBackoff: 5 * time.Millisecond}

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		claim := claimNextReady(t, store, "worker", lease)
		if claim.JobID != jobID || claim.AttemptCount != attempt {
			t.Fatalf("attempt %d claim = %+v, want job %s attempt %d", attempt, claim, jobID, attempt)
		}
		dead, err := store.Fail(ctx, claim, policy)
		if err != nil {
			t.Fatalf("Fail() at attempt %d error = %v", attempt, err)
		}
		snap := readSnapshot(t, store, tenant, jobID)
		switch {
		case attempt < maxAttempts:
			if dead || snap.Status != "queued" {
				t.Fatalf("attempt %d: dead=%v status=%q, want requeued", attempt, dead, snap.Status)
			}
		default:
			if !dead || snap.Status != "dead" {
				t.Fatalf("final attempt: dead=%v status=%q, want dead-lettered", dead, snap.Status)
			}
		}
	}
	// A dead-lettered job is no longer claimable.
	if _, _, err := store.ClaimNext(ctx, "worker", lease); !errors.Is(err, coordinator.ErrNoClaimableJob) {
		t.Fatalf("ClaimNext() after dead-letter error = %v, want ErrNoClaimableJob", err)
	}
}

// TestReconcilerDeadLettersAbandonedBeyondMaxAttempts proves the expiry sweep is the
// safety net for a worker that is killed every attempt and never self-reports: once
// the lease has lapsed and the attempts are exhausted, the reconciler dead-letters
// the job so it cannot be reclaimed forever.
func TestReconcilerDeadLettersAbandonedBeyondMaxAttempts(t *testing.T) {
	store := openStore(t)
	tenant := seedTenant(t, store.Pool())
	jobID := enqueueJob(t, store, tenant)
	lease := faultLease(t)
	ctx := context.Background()

	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if _, err := store.Claim(ctx, tenant, jobID, "worker-killed", lease); err != nil {
			t.Fatalf("attempt %d Claim() error = %v", attempt, err)
		}
		waitLeaseExpiry(t, store, tenant, jobID) // the worker died holding the lease
	}

	swept, err := store.ReclaimExpired(ctx, maxAttempts)
	if err != nil {
		t.Fatalf("ReclaimExpired() error = %v", err)
	}
	if swept < 1 {
		t.Fatalf("ReclaimExpired() swept %d, want >= 1", swept)
	}
	if snap := readSnapshot(t, store, tenant, jobID); snap.Status != "dead" {
		t.Fatalf("snapshot after reconcile = %+v, want dead-lettered", snap)
	}
	if _, _, err := store.ClaimNext(ctx, "worker", lease); !errors.Is(err, coordinator.ErrNoClaimableJob) {
		t.Fatalf("ClaimNext() after reconcile error = %v, want ErrNoClaimableJob", err)
	}
}

// claimNextReady polls ClaimNext until a job becomes ready, so a persisted backoff
// deadline (ready_at in the near future) is respected without a wall-clock sleep.
func claimNextReady(t *testing.T, store *coordinator.Store, owner string, lease time.Duration) coordinator.Claim {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		claim, _, err := store.ClaimNext(ctx, owner, lease)
		if err == nil {
			return claim
		}
		if !errors.Is(err, coordinator.ErrNoClaimableJob) {
			t.Fatalf("ClaimNext() error = %v", err)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("no claimable job before deadline: %v", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
}
