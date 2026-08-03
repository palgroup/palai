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

-- InsertInboundArtifact records an artifact whose id the CALLER chose, run_id still unset, and
-- does nothing if that id already exists. Both halves are for one caller shape: an integration
-- that admitted a shared file (E19/E20 Slack).
--
-- WHY THE CALLER CHOOSES THE ID. The run's stored input names the artifact, and the admission's
-- request hash covers that input (slackRequestHash). A random id would make Slack's redelivery of
-- one event hash DIFFERENTLY from the original, so the idempotency reservation would report a
-- CONFLICT instead of replaying — the exact opposite of SLK-002. A caller-derived id that is a pure
-- function of the source event keeps the redelivery a replay. ON CONFLICT DO NOTHING is the other
-- half of that: the redelivery re-derives the same id and must find the row, not fail on it.
--
-- WHY run_id IS NULL HERE. The row has to exist BEFORE the admission, because the admission commits
-- the run's dispatch outbox in its own transaction — an artifact written after it races the model
-- step that reads it. But runs.id does not exist yet either, and artifacts.run_id references it. So
-- the row lands unattached and AttachArtifactRun binds it the moment the run is real.
-- name: InsertInboundArtifact
INSERT INTO artifacts (id, organization_id, project_id, object_key, size_bytes, checksum,
    media_type, logical_type, malware_scan_status, provenance)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
ON CONFLICT (id) DO NOTHING;

-- AttachArtifactRun binds an inbound artifact to the run that was admitted for it, tenant-scoped.
-- It is what puts the row inside RETENTION's reach: the §22.2 purge reaches an artifact through
-- `artifacts JOIN runs ON artifacts.run_id = runs.id`, so an unattached row is a row retention
-- cannot see. Only ever widens NULL -> a run: the guard makes it idempotent under a redelivery and
-- makes re-pointing an existing artifact at another run impossible.
-- name: AttachArtifactRun
UPDATE artifacts SET run_id = $3
WHERE id = $1 AND project_id = $2 AND run_id IS NULL;

-- GetArtifact reads an artifact's row within the tenant scope. An unknown or foreign id
-- returns no row, which the caller renders as a miss (404) — a foreign tenant cannot tell
-- a real artifact apart from a missing one, so the read leaks no cross-tenant existence.
-- name: GetArtifact
SELECT run_id, object_key, size_bytes, checksum
FROM artifacts
WHERE id = $1 AND project_id = $2;

-- ArtifactByID reads one artifact's full retrieval metadata within the tenant scope (E13 T5). Like
-- GetArtifact it returns no row for an unknown or foreign id, which the retrieval API renders as a 404
-- — a foreign tenant cannot tell a real artifact from a missing one, so the read leaks no cross-tenant
-- existence (§22.6 non-disclosure). object_key names the S3 object the content download streams;
-- size_bytes/checksum carry integrity; media_type/logical_type/malware_scan_status are the §22.6
-- classification the metadata projection surfaces.
-- name: ArtifactByID
SELECT run_id, object_key, size_bytes, checksum, media_type, logical_type, malware_scan_status, created_at
FROM artifacts
WHERE id = $1 AND project_id = $2;

-- ListArtifactsByRun lists a run's artifacts within the tenant scope (E13 T5, the run-scoped retrieval
-- list). Tenant-scoped like every artifact read; a run with no artifacts returns zero rows (an empty
-- list, not a miss). Ordered by created_at then id for a stable listing. ponytail: a run's artifact set
-- is small and bounded (a patch + a test log today), so this is unpaginated — cursor pagination is a
-- later concern if a run ever produces many.
-- name: ListArtifactsByRun
SELECT id, run_id, size_bytes, checksum, media_type, logical_type, malware_scan_status, created_at
FROM artifacts
WHERE run_id = $1 AND project_id = $2
ORDER BY created_at, id;

-- ReferencedArtifactObjectKeys lists every object key a live artifacts row still points at
-- — the reference set the orphan GC subtracts from the bucket listing (E10 Task 3). A
-- tombstoned row (retention cleared object_key to '') is excluded by the non-empty filter,
-- so its once-referenced object is reclaimed like any other orphan. Deliberately bucket-wide
-- with NO tenant scope: the GC's delete decision is the pure absence of a referencing row,
-- and the set must be complete across every tenant or GC could delete a live foreign object.
-- name: ReferencedArtifactObjectKeys
SELECT object_key FROM artifacts WHERE object_key <> '';
