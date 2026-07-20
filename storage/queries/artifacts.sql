-- Artifact write-path queries (spec §22.6, LP §7.2). Every query is tenant-scoped:
-- without organization and project a read returns no row, so a caller cannot reach
-- another tenant's artifact by guessing an id (the existence-non-disclosure rule the
-- retrieval path already enforces for responses).

-- InsertArtifact records an immutable artifact row after its bytes are committed to the
-- object store. object_key names the S3 object; size_bytes/checksum let a later read
-- verify integrity without re-fetching. The row is the durable index; the bytes live in
-- the control-plane-only object store (spec §24 — the engine/runner never see either).
-- name: InsertArtifact
INSERT INTO artifacts (id, organization_id, project_id, run_id, object_key, size_bytes, checksum)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- GetArtifact reads an artifact's row within the tenant scope. An unknown or foreign id
-- returns no row, which the caller renders as a miss (404) — a foreign tenant cannot tell
-- a real artifact apart from a missing one, so the read leaks no cross-tenant existence.
-- name: GetArtifact
SELECT run_id, object_key, size_bytes, checksum
FROM artifacts
WHERE id = $1 AND organization_id = $2 AND project_id = $3;
