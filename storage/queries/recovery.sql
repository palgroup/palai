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
