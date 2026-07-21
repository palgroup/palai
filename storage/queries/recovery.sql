-- name: InsertTranscriptBoundary
-- The shared recovery boundary (spec §26.1). Idempotent on id: re-anchoring at the same boundary
-- reuses the row rather than duplicating it, so the checkpoint's FK always resolves. transcript_sequence
-- is the journal event seq (events.seq) where the canonical transcript stood at the cut.
INSERT INTO transcript_boundaries
    (id, run_id, attempt_id, organization_id, project_id, transcript_sequence)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (id) DO NOTHING;

-- name: InsertCheckpoint
-- Record an IMMUTABLE checkpoint (spec §26.2). A second write to the same id raises a unique_violation
-- (23505) the caller maps to ErrCheckpointExists — the DB-level immutability guard (§26.1); there is no
-- UPDATE path (the role GRANT withholds it). pending_operations uses the table default '[]' (T7 fills it);
-- workspace_snapshot_id is NULL when the checkpoint declares no workspace dependency (§26.4).
INSERT INTO checkpoints
    (id, run_id, attempt_id, boundary_id, organization_id, project_id,
     engine_digest, engine_version, protocol_version, format, format_version,
     config_snapshot_hash, transcript_sequence, workspace_snapshot_id,
     content_checksum, object_key, size_bytes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17);

-- name: LatestRunCheckpoint
-- The recovery ladder's read (spec §26.3-26.4, E10 T4): a run's NEWEST checkpoint with the §26.4
-- compatibility inputs. Index-backed by checkpoints_by_run (run_id, created_at DESC). No row for a
-- run without a checkpoint -> the caller falls to transcript reconstruction, never a phantom restore.
SELECT id, boundary_id, attempt_id, format, format_version, config_snapshot_hash,
       protocol_version, transcript_sequence, workspace_snapshot_id, content_checksum,
       object_key, size_bytes
FROM checkpoints
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY created_at DESC
LIMIT 1;

-- name: ReferencedCheckpointObjectKeys
-- The checkpoint half of the orphan-GC reference set (E10 T1<->T3). Checkpoint bytes live in the
-- SAME bucket as artifacts under <org>/<proj>/<run>/checkpoints/<id> but are tracked here, NOT in
-- artifacts — so the GC must UNION these keys with ReferencedArtifactObjectKeys, or it reclaims
-- every live checkpoint as an orphan and destroys the recovery bytes T1 depends on. Deliberately
-- bucket-wide with NO tenant scope, matching the artifacts query: the delete decision is the pure
-- absence of a referencing row, so the set must be complete across every tenant. The <> '' filter
-- mirrors the artifacts query (checkpoints are immutable, so no tombstone-scrub clears object_key).
SELECT object_key FROM checkpoints WHERE object_key <> '';
