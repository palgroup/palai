-- Session chaining queries (spec §9, §22.1). Every read is tenant-scoped: a session or
-- response id from another tenant matches no row, so an unknown/foreign id renders the
-- same 404 as retrieval and never leaks a cross-tenant resource's existence (§39.2).

-- SessionForCreate resolves a caller-supplied session_id within the tenant scope, returning
-- its lifecycle state so admission can append to an active session or reject a closed one.
-- name: SessionForCreate
SELECT id, state
FROM sessions
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- SessionForPreviousResponse resolves the session a previous_response_id continues, joined so
-- one round-trip yields both the session id and its lifecycle state. An unknown/foreign
-- response matches no row (404).
-- name: SessionForPreviousResponse
SELECT s.id, s.state
FROM responses r
JOIN sessions s ON s.id = r.session_id
WHERE r.id = $1 AND r.organization_id = $2 AND r.project_id = $3;

-- SessionHistory returns the prior responses of a session in creation order so run.start can
-- carry them as conversation history (spec §22.2). A retained response yields its stored
-- output projection; a purged one yields NULL output with purged = true, which the assembler
-- renders as a redacted_content marker. Ordered by created_at (id tiebreak) — ponytail:
-- created_at ordering, responses carry no per-session ordinal of their own; add one if a
-- sub-microsecond clock tie ever reorders a chain.
-- name: SessionHistory
SELECT output, purged_at IS NOT NULL AS purged
FROM responses
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
  AND created_at < (SELECT created_at FROM responses WHERE id = $4)
ORDER BY created_at, id;
