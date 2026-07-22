-- Artifact write-path queries (spec §22.6, LP §7.2). Every query is tenant-scoped:
-- without organization and project a read returns no row, so a caller cannot reach
-- another tenant's artifact by guessing an id (the existence-non-disclosure rule the
-- retrieval path already enforces for responses).

-- InsertArtifact records an immutable artifact row after its bytes are committed to the
-- object store. object_key names the S3 object; size_bytes/checksum let a later read
-- verify integrity without re-fetching. media_type/logical_type classify the object and
-- provenance links it to its producer (spec §22.6); malware_scan_status carries the scan
-- outcome ('not_scanned' until a scanner is wired). The row is the durable index; the bytes
-- live in the control-plane-only object store (spec §24 — the engine/runner never see either).
-- name: InsertArtifact
INSERT INTO artifacts (id, organization_id, project_id, run_id, object_key, size_bytes, checksum,
    media_type, logical_type, malware_scan_status, provenance)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11);

-- GetArtifact reads an artifact's row within the tenant scope. An unknown or foreign id
-- returns no row, which the caller renders as a miss (404) — a foreign tenant cannot tell
-- a real artifact apart from a missing one, so the read leaks no cross-tenant existence.
-- name: GetArtifact
SELECT run_id, object_key, size_bytes, checksum
FROM artifacts
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ArtifactByID reads one artifact's full retrieval metadata within the tenant scope (E13 T5). Like
-- GetArtifact it returns no row for an unknown or foreign id, which the retrieval API renders as a 404
-- — a foreign tenant cannot tell a real artifact from a missing one, so the read leaks no cross-tenant
-- existence (§22.6 non-disclosure). object_key names the S3 object the content download streams;
-- size_bytes/checksum carry integrity; media_type/logical_type/malware_scan_status are the §22.6
-- classification the metadata projection surfaces.
-- name: ArtifactByID
SELECT run_id, object_key, size_bytes, checksum, media_type, logical_type, malware_scan_status, created_at
FROM artifacts
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListArtifactsByRun lists a run's artifacts within the tenant scope (E13 T5, the run-scoped retrieval
-- list). Tenant-scoped like every artifact read; a run with no artifacts returns zero rows (an empty
-- list, not a miss). Ordered by created_at then id for a stable listing. ponytail: a run's artifact set
-- is small and bounded (a patch + a test log today), so this is unpaginated — cursor pagination is a
-- later concern if a run ever produces many.
-- name: ListArtifactsByRun
SELECT id, run_id, size_bytes, checksum, media_type, logical_type, malware_scan_status, created_at
FROM artifacts
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY created_at, id;

-- ReferencedArtifactObjectKeys lists every object key a live artifacts row still points at
-- — the reference set the orphan GC subtracts from the bucket listing (E10 Task 3). A
-- tombstoned row (retention cleared object_key to '') is excluded by the non-empty filter,
-- so its once-referenced object is reclaimed like any other orphan. Deliberately bucket-wide
-- with NO tenant scope: the GC's delete decision is the pure absence of a referencing row,
-- and the set must be complete across every tenant or GC could delete a live foreign object.
-- name: ReferencedArtifactObjectKeys
SELECT object_key FROM artifacts WHERE object_key <> '';
