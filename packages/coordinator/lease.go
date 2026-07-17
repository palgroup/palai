package coordinator

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// reclaimBatch bounds one reconciler sweep so a backlog of abandoned jobs cannot
// take a table-wide lock or a runaway transaction.
const reclaimBatch = 100

// deadLetterBatch bounds one dead-letter bridge sweep, mirroring reclaimBatch.
const deadLetterBatch = 100

// deadLetterProjection is the terminal Response body a dead-lettered run finalizes to:
// no output and no usage, because its attempts never produced any.
var deadLetterProjection = []byte(`{"output":[],"usage":{}}`)

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

// deadLetteredRun is one dead-lettered response.run job's run and response to fail.
type deadLetteredRun struct {
	tenant     Tenant
	runID      string
	responseID string
}

// SweepDeadLetteredRuns drives every dead-lettered response.run job whose run is still
// non-terminal to a failed run terminal (spec §24.4 -> §22.3). A run whose every attempt
// hit a deterministic engine/protocol violation exhausts its ceiling and its durable job
// dead-letters, but the run never self-reports, so without this bridge the response hangs
// in running and its SSE stream never closes. Each run is driven with RunCmdFail — the
// transition, the run.failed.v1 terminal event, and its outbox row commit together in
// ApplyRunTransition — and its response projection is finalized to failed so a retrieval
// reads a terminal failure. It is idempotent: a run already terminal is excluded by the
// query and, if it raced there, skipped by the transition, so terminal monotonicity holds
// and a job an operator later retries finds the run terminal and changes nothing. The dead
// job is left dead for operator retry/reconcile actions. Bounded per sweep; returns the
// number driven to failed.
func (s *Store) SweepDeadLetteredRuns(ctx context.Context) (int, error) {
	rows, err := s.pool.Query(ctx, storage.Query("DeadLetteredResponseRuns"), deadLetterBatch)
	if err != nil {
		return 0, fmt.Errorf("query dead-lettered runs: %w", err)
	}
	var dead []deadLetteredRun
	for rows.Next() {
		var d deadLetteredRun
		if err := rows.Scan(&d.tenant.Organization, &d.tenant.Project, &d.runID, &d.responseID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan dead-lettered run: %w", err)
		}
		dead = append(dead, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read dead-lettered runs: %w", err)
	}

	// The cursor is drained before any transition, so driving each run acquires its own
	// pooled connection without contending with the open scan.
	driven := 0
	for _, d := range dead {
		switch _, err := s.ApplyRunTransition(ctx, d.tenant, d.runID, statemachines.RunCmdFail); {
		case errors.Is(err, ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
			continue // already terminal or past a failable state — idempotent
		case err != nil:
			return driven, fmt.Errorf("drive run %s to failed: %w", d.runID, err)
		}
		if err := s.FinalizeResponse(ctx, d.tenant, d.responseID, string(statemachines.RunFailed), deadLetterProjection); err != nil {
			return driven, err
		}
		driven++
	}
	return driven, nil
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
