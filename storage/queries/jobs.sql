-- Durable coordinator queries (spec §24.4). Claim eligibility and every timestamp
-- use the database clock, never a worker clock, so a paused host cannot self-certify
-- a live lease.

-- name: EnqueueJob
INSERT INTO durable_jobs (id, organization_id, project_id, kind, payload)
VALUES ($1, $2, $3, $4, $5);

-- name: ClaimJob
WITH claimable AS (
    SELECT id
    FROM durable_jobs
    WHERE id = $1
      AND organization_id = $2
      AND project_id = $3
      AND ready_at <= clock_timestamp()
      AND (
        status = 'queued'
        OR (status = 'running' AND lease_expires_at <= clock_timestamp())
      )
    FOR UPDATE SKIP LOCKED
)
UPDATE durable_jobs AS job
SET status = 'running',
    lease_owner = $4,
    lease_expires_at = clock_timestamp() + ($5::bigint * interval '1 millisecond'),
    fence = job.fence + 1,
    attempt_count = job.attempt_count + 1,
    updated_at = clock_timestamp()
FROM claimable
WHERE job.id = claimable.id
RETURNING job.id, job.lease_owner, job.fence, job.attempt_count, job.lease_expires_at;

-- name: RecordJobAttempt
INSERT INTO job_attempts (job_id, fence, owner)
VALUES ($1, $2, $3);

-- name: JobLeaseExpired
SELECT lease_expires_at IS NOT NULL AND clock_timestamp() >= lease_expires_at
FROM durable_jobs
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: CompleteJob
UPDATE durable_jobs
SET status = 'completed',
    lease_owner = NULL,
    lease_expires_at = NULL,
    result_hash = $5,
    updated_at = clock_timestamp()
WHERE id = $1
  AND organization_id = $2
  AND project_id = $3
  AND fence = $4
  AND lease_owner IS NOT NULL
  AND status = 'running';

-- name: JobSnapshot
SELECT status, fence, attempt_count, result_hash
FROM durable_jobs
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: ClaimNextJob
-- The worker loop leases the oldest ready job across the queue. The job queue is
-- coordinator infrastructure, so a claim spans tenants and returns the job's own
-- scope for the handler to act within. One row is taken per call (bounded batch);
-- rows locked by peer workers are skipped. Eligibility and the lease deadline use
-- the database clock, and every claim raises a monotonic fence and attempt count.
WITH claimable AS (
    SELECT id
    FROM durable_jobs
    WHERE ready_at <= clock_timestamp()
      AND (
        status = 'queued'
        OR (status = 'running' AND lease_expires_at <= clock_timestamp())
      )
    ORDER BY ready_at
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE durable_jobs AS job
SET status = 'running',
    lease_owner = $1,
    lease_expires_at = clock_timestamp() + ($2::bigint * interval '1 millisecond'),
    fence = job.fence + 1,
    attempt_count = job.attempt_count + 1,
    updated_at = clock_timestamp()
FROM claimable
WHERE job.id = claimable.id
RETURNING job.id, job.organization_id, job.project_id, job.lease_owner,
          job.fence, job.attempt_count, job.lease_expires_at, job.payload;

-- name: HeartbeatJob
-- Renew a live lease by database time. Fenced to the exact holder: once the job is
-- reclaimed at a higher fence the predicate matches nothing, so a paused worker
-- cannot resurrect a lost lease.
UPDATE durable_jobs
SET lease_expires_at = clock_timestamp() + ($4::bigint * interval '1 millisecond'),
    updated_at = clock_timestamp()
WHERE id = $1
  AND fence = $2
  AND lease_owner = $3
  AND status = 'running'
RETURNING lease_expires_at;

-- name: FailJob
-- Record a failed attempt: requeue the job behind a persisted backoff deadline, or
-- dead-letter it once its attempts are exhausted. The attempt count is canonical in
-- the row, so the ceiling is enforced here rather than in a worker's memory. Fenced
-- to the holder; a superseded worker matches nothing and mutates no state.
UPDATE durable_jobs
SET status = CASE WHEN attempt_count >= $4 THEN 'dead' ELSE 'queued' END,
    lease_owner = NULL,
    lease_expires_at = NULL,
    ready_at = CASE WHEN attempt_count >= $4 THEN ready_at
                    ELSE clock_timestamp() + ($5::bigint * interval '1 millisecond') END,
    updated_at = clock_timestamp()
WHERE id = $1
  AND fence = $2
  AND lease_owner = $3
  AND status = 'running'
RETURNING status;

-- name: RecordJobOutcome
-- Close the attempt ledger row so the durable history records how each fenced
-- attempt ended, not merely that it started.
UPDATE job_attempts
SET outcome = $3
WHERE job_id = $1 AND fence = $2;

-- name: DeadLetteredResponseRuns
-- Reconciler bridge (spec §24.4 -> §22.3): a response.run job that dead-lettered while
-- its run is still non-terminal names a run that will otherwise hang in running forever —
-- its response never projects terminal and its SSE stream never closes. Return each such
-- run and its response, tenant-scoped and bounded per sweep. Runs already terminal are
-- excluded (only the states RunCmdFail is legal from), so a run failed by an earlier sweep
-- is never reprocessed and terminal monotonicity holds.
SELECT j.organization_id, j.project_id, j.payload->>'run_id' AS run_id, r.response_id
FROM durable_jobs j
JOIN runs r
  ON r.id = j.payload->>'run_id'
 AND r.organization_id = j.organization_id
 AND r.project_id = j.project_id
WHERE j.status = 'dead'
  AND j.kind = 'response.run'
  AND r.state IN ('queued', 'provisioning', 'running', 'waiting')
ORDER BY j.updated_at
LIMIT $1;

-- name: ReclaimExpiredJobs
-- Reconciler safety net: dead-letter jobs whose lease has lapsed and whose attempts
-- are exhausted — the abandoned-work case where a worker is killed every attempt and
-- never self-reports. Expired leases still under the ceiling are left for the next
-- claim, which reclaims them inline at a higher fence. Bounded per sweep.
WITH abandoned AS (
    SELECT id
    FROM durable_jobs
    WHERE status = 'running'
      AND lease_expires_at <= clock_timestamp()
      AND attempt_count >= $1
    ORDER BY lease_expires_at
    FOR UPDATE SKIP LOCKED
    LIMIT $2
)
UPDATE durable_jobs AS job
SET status = 'dead',
    lease_owner = NULL,
    lease_expires_at = NULL,
    updated_at = clock_timestamp()
FROM abandoned
WHERE job.id = abandoned.id;
