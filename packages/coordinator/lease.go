package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// reclaimBatch bounds one reconciler sweep so a backlog of abandoned jobs cannot
// take a table-wide lock or a runaway transaction.
const reclaimBatch = 100

// RetryPolicy bounds how a failed job is retried before it is dead-lettered. A
// failure recorded at or beyond MaxAttempts dead-letters the job; otherwise the job
// is requeued after a full-jitter backoff, persisted as the row's ready_at deadline.
type RetryPolicy struct {
	MaxAttempts int
	BaseBackoff time.Duration
	MaxBackoff  time.Duration
}

// Heartbeat renews a live claim's lease by database time and returns the new expiry.
// It is fenced to the exact holder: a claim whose fence no longer owns the job (it
// was reclaimed after this worker stalled) renews nothing and returns ErrStaleFence,
// so a paused host cannot resurrect a lost lease.
func (s *Store) Heartbeat(ctx context.Context, claim Claim, extend time.Duration) (time.Time, error) {
	if claim.JobID == "" || claim.Fence < 1 || claim.Owner == "" || extend <= 0 {
		return time.Time{}, errors.New("valid claim and positive extension are required")
	}
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, storage.Query("HeartbeatJob"),
		claim.JobID, claim.Fence, claim.Owner, extend.Milliseconds()).Scan(&expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, ErrStaleFence
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("heartbeat job: %w", err)
	}
	return expiresAt, nil
}

// Fail records a failed attempt and either requeues the job behind a full-jitter
// backoff deadline or dead-letters it once its attempts are exhausted. The attempt
// count is canonical in the row, so the worker never hidden-retries: the ceiling is
// enforced by the row, not by worker memory. It is fenced — a superseded holder
// mutates nothing and returns ErrStaleFence — and it records the attempt's outcome in
// the ledger in the same transaction. The bool reports whether the job was
// dead-lettered.
func (s *Store) Fail(ctx context.Context, claim Claim, policy RetryPolicy) (bool, error) {
	if claim.JobID == "" || claim.Fence < 1 || claim.Owner == "" {
		return false, errors.New("valid claim is required")
	}
	if policy.MaxAttempts < 1 {
		return false, errors.New("retry policy requires a positive MaxAttempts")
	}
	backoff := FullJitterBackoff(claim.AttemptCount, policy.BaseBackoff, policy.MaxBackoff)

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin fail: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var status string
	err = tx.QueryRow(ctx, storage.Query("FailJob"),
		claim.JobID, claim.Fence, claim.Owner, policy.MaxAttempts, backoff.Milliseconds()).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ErrStaleFence
	}
	if err != nil {
		return false, fmt.Errorf("fail job: %w", err)
	}
	outcome := "failed"
	if status == "dead" {
		outcome = "dead"
	}
	if _, err := tx.Exec(ctx, storage.Query("RecordJobOutcome"), claim.JobID, claim.Fence, outcome); err != nil {
		return false, fmt.Errorf("record job outcome: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit fail: %w", err)
	}
	return status == "dead", nil
}

// ReclaimExpired dead-letters jobs whose lease has lapsed and whose attempts are
// exhausted — the safety net for workers killed every attempt that never self-report.
// An expired lease still under its ceiling is left for the next claim, which reclaims
// it inline at a higher fence. The sweep is bounded per call and returns the number
// dead-lettered.
func (s *Store) ReclaimExpired(ctx context.Context, maxAttempts int) (int, error) {
	if maxAttempts < 1 {
		return 0, errors.New("maxAttempts must be positive")
	}
	tag, err := s.pool.Exec(ctx, storage.Query("ReclaimExpiredJobs"), maxAttempts, reclaimBatch)
	if err != nil {
		return 0, fmt.Errorf("reclaim expired jobs: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// FullJitterBackoff returns a random duration in [0, min(max, base*2^(attempt-1))] —
// the AWS "full jitter" schedule, which spreads retries so a fleet of workers does
// not synchronize its reclaim storms. attempt is 1-based; a non-positive base
// disables backoff (immediate retry). The exponential ceiling stops doubling once it
// reaches max, so a large attempt count cannot overflow.
func FullJitterBackoff(attempt int, base, max time.Duration) time.Duration {
	if base <= 0 || attempt < 1 {
		return 0
	}
	ceiling := base
	for i := 1; i < attempt && ceiling < max; i++ {
		ceiling *= 2
	}
	if max > 0 && ceiling > max {
		ceiling = max
	}
	return time.Duration(rand.Int64N(int64(ceiling) + 1))
}
