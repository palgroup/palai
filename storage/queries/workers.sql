-- CapabilityWorker contract persistence (spec §31.2-31.6, E17 Task 9, WRK-001..007). capability_workers is
-- the enrolled-worker registry (mutable health/heartbeat/lease_fence). capability_jobs is the APPEND-ONLY job
-- journal: one immutable row per lifecycle entry, keyed (job_id, entry_seq); a job's current state is its
-- highest-seq entry. Every query is tenant-scoped (RLS 000029/000039 + the org/project predicate as defence
-- in depth): the tenant comes from the enrolled worker's own verified scope, never from anything the worker
-- sends. secret_handle_refs is only ref NAMES — a secret VALUE is never written to this table.

-- name: InsertCapabilityWorker
INSERT INTO capability_workers (
    id, organization_id, project_id, capability, capability_version, os, arch,
    toolchain_digests, capacity, pool_label, trust_label, health, lease_fence)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'healthy', 1);

-- name: GetCapabilityWorker
SELECT id, organization_id, project_id, capability, capability_version, os, arch,
       toolchain_digests, capacity, pool_label, trust_label, health, lease_fence
FROM capability_workers
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SetCapabilityWorkerHealth records a health/capability change and BUMPS lease_fence, so any lease the worker
-- still holds is fenced out (§31.6: a health/capability change cuts the new lease). Returns the new fence.
-- name: SetCapabilityWorkerHealth
UPDATE capability_workers
SET health = $4, lease_fence = lease_fence + 1, last_heartbeat_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING lease_fence;

-- name: HeartbeatCapabilityWorker
UPDATE capability_workers
SET last_heartbeat_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3 AND health = 'healthy'
RETURNING lease_fence;

-- AppendCapabilityJobEntry inserts one IMMUTABLE journal entry, computing entry_seq atomically as the job's
-- current max + 1. Two concurrent appends compute the same seq and collide on UNIQUE(job_id, entry_seq)
-- (23505), so a claim/terminal write is at-most-once — the queue_messages fence precedent. A first dispatch
-- (max is 0 -> seq 1) additionally trips the idempotency partial-unique index on a duplicate key.
-- name: AppendCapabilityJobEntry
INSERT INTO capability_jobs (
    id, organization_id, project_id, job_id, entry_seq, entry_kind, idempotency_key, run_id, attempt_id,
    worker_id, capability, operation, input_refs, secret_handle_refs, deadline_at, resource_limits,
    output_schema, network_policy, side_effect_key, fence_token, receipt)
SELECT $1, $2, $3, $4,
       COALESCE((SELECT max(entry_seq) FROM capability_jobs WHERE job_id = $4), 0) + 1,
       $5, $6, $7, $8, $9, $10, $11, $12::jsonb, $13::jsonb, $14, $15::jsonb, $16::jsonb, $17::jsonb, $18, $19, $20::jsonb
RETURNING entry_seq, fence_token;

-- CurrentCapabilityJob resolves a job's CURRENT state for the §31.6 fence guard. current_fence is
-- MAX(fence_token) over the job's entries, which is MONOTONIC and tamper-evident: the journal is append-only,
-- so a stale worker can never LOWER it, and a re-dispatch raises it. A result/redeem whose claim fence is not
-- current_fence is rejected. entry_kind/worker_id are the latest-seq entry's; deadline_at is read
-- authoritatively from the DISPATCH entry (entry_seq = 1), never trusted from the worker's claim. A missing
-- job yields no rows (ErrNoSuchJob).
-- name: CurrentCapabilityJob
SELECT
    (SELECT max(fence_token) FROM capability_jobs f WHERE f.job_id = $1) AS current_fence,
    latest.entry_kind,
    latest.worker_id,
    (SELECT d.deadline_at FROM capability_jobs d WHERE d.job_id = $1 AND d.entry_seq = 1) AS deadline_at,
    (SELECT s.secret_handle_refs FROM capability_jobs s WHERE s.job_id = $1 AND s.entry_seq = 1) AS secret_handle_refs
FROM (
    SELECT entry_kind, worker_id
    FROM capability_jobs
    WHERE job_id = $1 AND organization_id = $2 AND project_id = $3
    ORDER BY entry_seq DESC
    LIMIT 1
) latest;

-- ReadyCapabilityJob picks ONE job ready for a worker of the given capability: its latest entry is
-- 'dispatched' (not yet leased/terminal) and its deadline has not passed. The caller then appends a 'leased'
-- entry at the dispatch fence. ponytail: DISTINCT ON scans the capability's journal per claim — fine at
-- fixture/reference scale; a ready-jobs materialized view or a status column is the upgrade if a real fleet
-- polls hard.
-- name: ReadyCapabilityJob
SELECT latest.job_id, latest.operation, latest.deadline_at, latest.fence_token, latest.input_refs,
       latest.secret_handle_refs, latest.resource_limits, latest.output_schema, latest.network_policy,
       latest.side_effect_key, latest.run_id, latest.attempt_id
FROM (
    SELECT DISTINCT ON (job_id) job_id, entry_kind, operation, deadline_at, fence_token, input_refs,
           secret_handle_refs, resource_limits, output_schema, network_policy, side_effect_key, run_id, attempt_id
    FROM capability_jobs
    WHERE organization_id = $1 AND project_id = $2 AND capability = $3
    ORDER BY job_id, entry_seq DESC
) latest
WHERE latest.entry_kind = 'dispatched'
  AND (latest.deadline_at IS NULL OR latest.deadline_at > clock_timestamp())
ORDER BY latest.job_id
LIMIT 1;

-- JobByIdempotencyKey resolves the job_id an idempotency key already dispatched (the idempotent-dispatch
-- lookup — a duplicate dispatch returns the existing job rather than a second one).
-- name: JobByIdempotencyKey
SELECT job_id
FROM capability_jobs
WHERE organization_id = $1 AND project_id = $2 AND idempotency_key = $3 AND entry_seq = 1
LIMIT 1;

-- JobDispatchSpec reads a job's immutable DISPATCH entry (entry_seq = 1) — the spec a re-dispatch reuses. It
-- is read regardless of the job's current kind (unlike ReadyCapabilityJob, which only returns dispatched-
-- latest jobs).
-- name: JobDispatchSpec
SELECT idempotency_key, run_id, attempt_id, capability, operation, input_refs, secret_handle_refs,
       deadline_at, side_effect_key, fence_token
FROM capability_jobs
WHERE job_id = $1 AND organization_id = $2 AND project_id = $3 AND entry_seq = 1;
