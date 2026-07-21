-- Response/run execution-state queries (spec §22.3). Every query is tenant-scoped:
-- without organization and project it returns no row, so a caller cannot reach
-- another tenant's run by guessing an ID.

-- name: LockRun
SELECT session_id, response_id, state
FROM runs
WHERE id = $1 AND organization_id = $2 AND project_id = $3
FOR UPDATE;

-- name: UpdateRunState
UPDATE runs
SET state = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- GetResponse reads a response's terminal projection for retrieval. purged_at is
-- non-null once the content has been reaped, which the handler renders as 410.
-- name: GetResponse
SELECT state, output, purged_at, created_at
FROM responses
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- RunContext resolves a run's durable context (tenant, session, response, input) by
-- its primary key. The run id is coordinator-supplied from the claimed job, so this
-- by-PK read establishes the scope every later write is gated by — the same
-- cross-tenant infrastructure read the job claim itself performs (spec §24.4).
-- name: RunContext
SELECT r.organization_id, r.project_id, r.session_id, r.response_id, r.state, resp.input
FROM runs r
JOIN responses resp ON resp.id = r.response_id
WHERE r.id = $1;

-- RunIDForResponse resolves a response's root run within the tenant scope. An unknown or
-- foreign id returns no row (the caller renders 404, never leaking cross-tenant existence).
-- LP's response:run is 1:1, so a response has exactly one run.
-- name: RunIDForResponse
SELECT id
FROM runs
WHERE response_id = $1 AND organization_id = $2 AND project_id = $3;

-- RunDelegation reads a run's delegation context (spec §25.18): its depth and the delegation
-- JSON — {"emit":[...]} on a root run configured to delegate, {"spec":{...}} on a child run,
-- NULL on a plain run. By primary key, like RunContext, so the orchestrator reads it once per
-- attempt to seed run.start delegations and route a child's own model/budget.
-- name: RunDelegation
SELECT depth, delegation
FROM runs
WHERE id = $1;

-- InsertChildRun creates a ChildRun (spec §25.18-19): a runs row carrying parent_run_id, its
-- depth, and its own delegation spec, in the parent's session. It is excluded from
-- one-active-root (parent_run_id IS NOT NULL), so it does not consume the session's root slot.
-- name: InsertChildRun
INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, parent_run_id, depth, delegation)
VALUES ($1, $2, $3, $4, $5, 'queued', $6, $7, $8);

-- ChildRunOutcome reads a finished ChildRun's terminal run state and its response projection,
-- so the parent can fold the child.result and link the child run. Tenant-scoped by primary key.
-- name: ChildRunOutcome
SELECT r.state, resp.output
FROM runs r
JOIN responses resp ON resp.id = r.response_id
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;

-- NonTerminalDescendantRuns walks the parent_run_id tree from a run and returns every
-- non-terminal descendant (spec §25.18 cancel propagation, SUB-005). Recursive so a cancel
-- reaches the whole subtree even if delegation depth grows past 1 later; today the depth cap
-- keeps it one level. Each descendant carries its response id so the caller finalizes it canceled.
-- name: NonTerminalDescendantRuns
WITH RECURSIVE subtree AS (
    SELECT id, response_id, state
    FROM runs
    WHERE parent_run_id = $1 AND organization_id = $2 AND project_id = $3
    UNION ALL
    SELECT c.id, c.response_id, c.state
    FROM runs c
    JOIN subtree s ON c.parent_run_id = s.id
    WHERE c.organization_id = $2 AND c.project_id = $3
)
SELECT id, response_id
FROM subtree
WHERE state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded');

-- LookupChildByRequest resolves the child a parent already spawned for a child_request_id (E10 T8,
-- DET-001): a restored parent re-emits the SAME deterministic child.request, so the controller must
-- RE-BIND the existing child rather than clone it. The linkage rides the child's delegation.spec JSONB
-- (child_request_id + the detached flag) — no separate column, no migration. Tenant-scoped; one row.
-- name: LookupChildByRequest
SELECT id, state,
       coalesce((delegation->'spec'->>'detached')::boolean, false),
       coalesce((delegation->'spec'->>'budget')::int, 0)
FROM runs
WHERE parent_run_id = $1 AND organization_id = $2 AND project_id = $3
  AND delegation->'spec'->>'child_request_id' = $4
LIMIT 1;

-- RunParentRun reads a run's parent_run_id (NULL for a root run) so the child-terminal wake can find
-- the parent to resume (E10 T8, DET-001). Tenant-scoped by primary key.
-- name: RunParentRun
SELECT parent_run_id
FROM runs
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- HasNonTerminalChildRun reports whether a parent still has any non-terminal child (E10 T8): the
-- detached-parent wake only re-enters the parent once EVERY child is terminal, so its resume re-emits
-- each child.request to a finished child (a clean fold, never a re-spawn). Direct children only —
-- depth is capped at 1, so a recursive walk is unnecessary here.
-- name: HasNonTerminalChildRun
SELECT EXISTS (
    SELECT 1 FROM runs
    WHERE parent_run_id = $1 AND organization_id = $2 AND project_id = $3
      AND state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
);

-- UpdateResponse writes the terminal Response projection (status + output/usage JSON). The
-- terminal states are excluded from the WHERE so the projection is monotonically terminal at the
-- database: the first terminal write wins and a late-arriving one (a reclaimed or in-flight
-- attempt whose run.terminal lands after a cancel) affects zero rows (spec §22.3). This is the
-- permanent, kill-safe class-fix for the 2-tx cancel window the e08a898 app-guard patched.
-- name: UpdateResponse
UPDATE responses
SET state = $4, output = $5, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3
  AND state NOT IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded');

-- UpsertToolCall records a completed tool call. ON CONFLICT DO NOTHING makes a
-- redelivered tool_call_id idempotent: the cached completion is authoritative and is
-- never overwritten (spec §26.7).
-- name: UpsertToolCall
INSERT INTO tool_calls (id, organization_id, project_id, run_id, fence, state, name, arguments, result)
VALUES ($1, $2, $3, $4, $5, 'completed', $6, $7, $8)
ON CONFLICT (id) DO NOTHING;

-- InsertModelRequest records a model request before the provider is called. It returns
-- the id only on a fresh insert, so the caller journals the request event exactly once
-- even if a reclaimed attempt re-derives the same stable id (spec §25.9, §53.4).
-- name: InsertModelRequest
INSERT INTO model_requests (id, organization_id, project_id, run_id, state)
VALUES ($1, $2, $3, $4, 'requested')
ON CONFLICT (id) DO NOTHING
RETURNING id;

-- GetModelResult reads a model request's state and committed result for replay.
-- name: GetModelResult
SELECT state, result
FROM model_requests
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- CommittedModelStepCount is the recovery replay watermark (spec §26.9, E10 T4): how many model
-- steps this run has already committed. On a run.start reconstruction the engine re-walks steps
-- 1..M (all replayed by LookupModelResult), so a fresh queued message must fold at the boundary that
-- precedes the FIRST live step (M+1), never into a replayed step's request. Committed steps are a
-- contiguous prefix, so the count IS the last replayed step's index.
-- name: SupersedeActiveAttempts
-- A new attempt on a run supersedes any prior NON-TERMINAL attempt, clearing the one-active-per-run
-- index so the new attempt can record. Marked 'preempted' (superseded by a newer attempt), NOT 'lost':
-- the predecessor may have cleanly paused or crashed, and this path does not know which — 'preempted'
-- is honest for the supersede itself, where 'lost' would falsely assert a crash. Only reachable once
-- the exact rung ruled out a still-live original (§26.3), so it never steals an active lease.
-- ponytail ceiling: a cleanly-SUCCEEDED attempt still has no path writing a terminal state on its row
-- (attempt-lifecycle terminal writes are T5/T6) — such a row is only reconciled here if a later attempt
-- supersedes it; the run's final attempt can linger non-terminal. Wire attempt.succeed on finalize in T5/T6.
UPDATE attempts SET state = 'preempted', updated_at = clock_timestamp()
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND id <> $4
  AND state IN ('assigned', 'starting', 'active', 'draining');

-- name: UpsertAttempt
-- Record the run attempt row (spec §26.1, E10 T4): the durable anchor the checkpoint /
-- transcript-boundary / workspace-snapshot FKs reference. Idempotent on id (a reclaim re-recording
-- the same attempt is a no-op). The fence is RUN-monotonic (MAX(fence)+1 over the run), NOT the job
-- claim fence: a resume mints a fresh job whose claim fence restarts at 1, so using it would collide
-- on the (run_id, fence) uniqueness — the run-scoped max keeps attempt fences strictly increasing
-- per run (spec §53.5). Runs under the supersede in one tx, and live attempts are serialized by the
-- exact rung, so the MAX read is race-free.
INSERT INTO attempts (id, organization_id, project_id, run_id, fence)
SELECT $1, $2, $3, $4, COALESCE(MAX(a.fence), 0) + 1
FROM attempts a
WHERE a.run_id = $4 AND a.organization_id = $2 AND a.project_id = $3
ON CONFLICT (id) DO NOTHING;

-- name: CommittedModelStepCount
SELECT count(*) FROM model_requests
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3 AND state = 'completed';

-- CompleteModelRequest stores the model result so a later attempt replays it.
-- name: CompleteModelRequest
UPDATE model_requests
SET state = 'completed', result = $4, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- Idempotent admission (spec §20.9, §8.3). The reservation is atomic with the
-- resource creation; the response_body is the exact resource a replay returns.

-- name: ReserveIdempotency
INSERT INTO idempotency_records
    (organization_id, project_id, principal_id, method, route, idempotency_key, request_hash, status, response_body)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'completed', $8)
ON CONFLICT (organization_id, project_id, principal_id, method, route, idempotency_key) DO NOTHING
RETURNING id;

-- GetIdempotency reads the reserved record for replay. result_purged_at is non-null
-- once the cached result has been reaped: a matching replay is then 410 with the
-- tombstone identity (resource_tombstone) rather than the (now absent) response_body.
-- name: GetIdempotency
SELECT request_hash, response_body, result_purged_at, resource_tombstone
FROM idempotency_records
WHERE organization_id = $1 AND project_id = $2 AND principal_id = $3
  AND method = $4 AND route = $5 AND idempotency_key = $6;

-- name: InsertSession
INSERT INTO sessions (id, organization_id, project_id)
VALUES ($1, $2, $3);

-- name: InsertResponse
INSERT INTO responses (id, organization_id, project_id, session_id, state, input, store)
VALUES ($1, $2, $3, $4, 'queued', $5, $6);

-- name: InsertRun
INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state, delegation)
VALUES ($1, $2, $3, $4, $5, 'queued', $6);

-- PurgeExpiredStoreFalse is the retention sweep (spec §8.3, §20.9): it purges the
-- content of store=false responses whose terminal state has aged past the configured
-- TTL, leaving a tombstone. One statement is one transaction; the victim set is
-- bounded (LIMIT) and taken FOR UPDATE SKIP LOCKED so a backlog cannot lock the table
-- or contend with a peer sweep. Every join carries the victim's own
-- organization/project, so a purge never crosses a tenant boundary. The data-modifying
-- CTEs read the victims' pre-purge content (all CTEs share one snapshot), so the
-- idempotency tombstone fingerprints the outcome before the row is scrubbed. $1 is the
-- TTL in milliseconds, $2 the batch bound. It returns one row: the count of purged
-- responses and the object keys of the artifacts it scrubbed, so the caller can delete
-- those bytes from the object store after this transaction commits (LP §7.2).
-- name: PurgeExpiredStoreFalse
WITH victims AS (
    SELECT id, organization_id, project_id, state, output
    FROM responses
    WHERE store = false
      AND purged_at IS NULL
      AND state IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
      AND updated_at < clock_timestamp() - ($1::bigint * interval '1 millisecond')
    ORDER BY updated_at
    FOR UPDATE SKIP LOCKED
    LIMIT $2
),
-- doomed_artifacts captures the victims' artifact object keys BEFORE purge_artifacts
-- scrubs object_key to '' in this same statement. Every WITH sub-statement runs on one
-- snapshot and cannot see another's writes, so this SELECT reads the pre-scrub keys. The
-- caller deletes these bytes after commit; a scrubbed row no longer names its object, so
-- surfacing the keys here is the only place the delete target survives.
doomed_artifacts AS (
    SELECT a.object_key
    FROM artifacts a
    JOIN runs r ON a.run_id = r.id
    JOIN victims v ON r.response_id = v.id
    WHERE a.organization_id = v.organization_id
      AND a.project_id = v.project_id
      AND a.object_key <> ''
),
tombstone AS (
    UPDATE idempotency_records i
    SET response_body = NULL,
        result_purged_at = clock_timestamp(),
        resource_tombstone = v.id,
        outcome_fingerprint = encode(sha256(convert_to(coalesce(v.state, '') || coalesce(v.output::text, ''), 'UTF8')), 'hex')
    FROM victims v
    WHERE i.response_body->>'id' = v.id
      AND i.organization_id = v.organization_id
      AND i.project_id = v.project_id
),
-- Per-response scrub (spec §22.2): only the victim response's own run-scoped events are
-- reaped, keyed by events.response_id (000003). A retained sibling response sharing the
-- session keeps its journal — the closure of the 000002 session-level scrub ceiling now
-- that a session chains multiple responses.
scrub_events AS (
    UPDATE events e
    SET payload = '{"purged": true}'::jsonb
    FROM victims v
    WHERE e.response_id = v.id
      AND e.organization_id = v.organization_id
      AND e.project_id = v.project_id
),
-- The row is scrubbed to an empty object_key (the DB index of the bytes is cleared); the
-- bytes themselves are deleted by the caller from the keys doomed_artifacts surfaced.
purge_artifacts AS (
    UPDATE artifacts a
    SET size_bytes = 0, object_key = '', checksum = ''
    FROM runs r, victims v
    WHERE a.run_id = r.id
      AND r.response_id = v.id
      AND a.organization_id = v.organization_id
      AND a.project_id = v.project_id
),
-- input is NOT NULL, so its customer content is scrubbed to an empty object rather than
-- nulled; output is nullable and cleared outright.
purged AS (
    UPDATE responses r
    SET input = '{}'::jsonb, output = NULL, purged_at = clock_timestamp()
    FROM victims v
    WHERE r.id = v.id
    RETURNING r.id
)
SELECT
    (SELECT count(*) FROM purged)::int AS purged_count,
    coalesce((SELECT array_agg(object_key) FROM doomed_artifacts), ARRAY[]::text[]) AS object_keys;
