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

-- name: GetResponse
SELECT session_id, state, output
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

-- name: GetIdempotency
SELECT request_hash, response_body
FROM idempotency_records
WHERE organization_id = $1 AND project_id = $2 AND principal_id = $3
  AND method = $4 AND route = $5 AND idempotency_key = $6;

-- name: InsertSession
INSERT INTO sessions (id, organization_id, project_id)
VALUES ($1, $2, $3);

-- name: InsertResponse
INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
VALUES ($1, $2, $3, $4, 'queued', $5);

-- name: InsertRun
INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
VALUES ($1, $2, $3, $4, $5, 'queued');
