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
