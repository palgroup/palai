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
