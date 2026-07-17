-- Response/run execution-state queries (spec §22.3). Every query is tenant-scoped:
-- without organization and project it returns no row, so a caller cannot reach
-- another tenant's run by guessing an ID.

-- name: LockRun
SELECT session_id, state
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
SELECT r.organization_id, r.project_id, r.session_id, r.response_id, resp.input
FROM runs r
JOIN responses resp ON resp.id = r.response_id
WHERE r.id = $1;

-- UpdateResponse writes the terminal Response projection (status + output/usage JSON).
-- name: UpdateResponse
UPDATE responses
SET state = $4, output = $5, updated_at = clock_timestamp()
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

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
INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
VALUES ($1, $2, $3, $4, $5, 'queued');

-- PurgeExpiredStoreFalse is the retention sweep (spec §8.3, §20.9): it purges the
-- content of store=false responses whose terminal state has aged past the configured
-- TTL, leaving a tombstone. One statement is one transaction; the victim set is
-- bounded (LIMIT) and taken FOR UPDATE SKIP LOCKED so a backlog cannot lock the table
-- or contend with a peer sweep. Every join carries the victim's own
-- organization/project, so a purge never crosses a tenant boundary. The data-modifying
-- CTEs read the victims' pre-purge content (all CTEs share one snapshot), so the
-- idempotency tombstone fingerprints the outcome before the row is scrubbed. $1 is the
-- TTL in milliseconds, $2 the batch bound.
-- name: PurgeExpiredStoreFalse
WITH victims AS (
    SELECT id, session_id, organization_id, project_id, state, output
    FROM responses
    WHERE store = false
      AND purged_at IS NULL
      AND state IN ('completed', 'failed', 'canceled', 'timed_out', 'budget_exceeded')
      AND updated_at < clock_timestamp() - ($1::bigint * interval '1 millisecond')
    ORDER BY updated_at
    FOR UPDATE SKIP LOCKED
    LIMIT $2
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
scrub_events AS (
    UPDATE events e
    SET payload = '{"purged": true}'::jsonb
    FROM victims v
    WHERE e.session_id = v.session_id
      AND e.organization_id = v.organization_id
      AND e.project_id = v.project_id
),
purge_artifacts AS (
    UPDATE artifacts a
    SET size_bytes = 0, object_key = '', checksum = ''
    FROM runs r, victims v
    WHERE a.run_id = r.id
      AND r.response_id = v.id
      AND a.organization_id = v.organization_id
      AND a.project_id = v.project_id
)
-- input is NOT NULL, so its customer content is scrubbed to an empty object rather than
-- nulled; output is nullable and cleared outright.
UPDATE responses r
SET input = '{}'::jsonb, output = NULL, purged_at = clock_timestamp()
FROM victims v
WHERE r.id = v.id;
