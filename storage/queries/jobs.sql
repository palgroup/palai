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
