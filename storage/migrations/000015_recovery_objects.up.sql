-- Durable recovery objects (spec §26.1-26.2, E10 Task 1). Recovery keeps three concerns SEPARATE:
-- the engine checkpoint (opaque loop state), the workspace snapshot (000008), and the canonical
-- transcript boundary. They share a recovery boundary id but have independent formats/retention —
-- restoring one never implies the others (§26.1). This migration adds the checkpoint + transcript
-- boundary halves and links the existing workspace_snapshots to the shared boundary.
--
-- Every CREATE ... / ADD COLUMN IF NOT EXISTS keeps the migration idempotent (Migrate is re-run per
-- boot), matching the 000008/000014 pattern. Tenant scope is the composite (organization_id,
-- project_id) FK to projects every execution row carries (spec §39.2).

-- The shared recovery boundary (spec §26.1): one anchor per boundary that the checkpoint, the
-- workspace snapshot, and the transcript state all reference. transcript_sequence is the journal
-- event seq (events.seq) at this boundary — where the canonical transcript stood when it was cut.
CREATE TABLE IF NOT EXISTS transcript_boundaries (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs (id),
    attempt_id TEXT NOT NULL REFERENCES attempts (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    transcript_sequence BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- Link the create-side snapshot (000008) to the shared boundary (spec §26.1). Nullable: a plain
-- create-side snapshot with no recovery boundary leaves it NULL; a snapshot cut AT a recovery
-- boundary references it. The FK bypasses on NULL, so legacy rows are unaffected.
ALTER TABLE workspace_snapshots
    ADD COLUMN IF NOT EXISTS boundary_id TEXT REFERENCES transcript_boundaries (id);

-- The engine checkpoint metadata (spec §26.2). The bytes live in the artifact store (object_key +
-- size_bytes + content_checksum); the control plane stores + checksums them but NEVER interprets
-- them (§26.2 — the engine boundary, §24). The row is IMMUTABLE: the id PK rejects a second write,
-- and the role GRANT below omits UPDATE, so a persisted checkpoint can never be silently rewritten.
CREATE TABLE IF NOT EXISTS checkpoints (
    id TEXT PRIMARY KEY,
    run_id TEXT NOT NULL REFERENCES runs (id),
    attempt_id TEXT NOT NULL REFERENCES attempts (id),
    boundary_id TEXT NOT NULL REFERENCES transcript_boundaries (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- §26.2 provenance. engine_digest/engine_version/protocol_version come from the attempt's pinned
    -- image + the engine.ready handshake; they default '' when not yet surfaced (honest-empty, never
    -- fabricated). NO "encrypted" claim here — envelope encryption is E13; T1 proves checksum +
    -- size-bound + secret-absence only.
    engine_digest TEXT NOT NULL DEFAULT '',
    engine_version TEXT NOT NULL DEFAULT '',
    protocol_version TEXT NOT NULL DEFAULT '',
    -- The reference-kernel format and its version (spec §26.4 compatibility). engine.ready advertises
    -- "<format>/<format_version>"; a restore accepts a checkpoint only if the target engine declares
    -- this pair compatible.
    format TEXT NOT NULL,
    format_version INTEGER NOT NULL,
    -- The effective ConfigSnapshot hash at the boundary (spec §26.2): a restore under a different
    -- config is caught by the §26.4 compatibility decision.
    config_snapshot_hash TEXT NOT NULL DEFAULT '',
    -- The journal event seq at the boundary (mirrors the shared boundary's, denormalized for the
    -- compatibility check without a join).
    transcript_sequence BIGINT NOT NULL,
    -- The matching workspace snapshot, or NULL when the checkpoint declares NO workspace dependency
    -- (spec §26.4). Independent lifecycles (§26.1), so this is a plain nullable FK, not a cascade.
    workspace_snapshot_id TEXT REFERENCES workspace_snapshots (id),
    -- Uncertain external operations in flight at the boundary (spec §26.2). Empty in T1 — the tool
    -- ledger that populates it is T7; the column is here so a producer never has to backfill it.
    pending_operations JSONB NOT NULL DEFAULT '[]',
    -- Content-addressed integrity over the opaque bytes ("sha256:<hex>"), plus where they live and
    -- how big they are (size-bounded before the PUT, spec §26.2).
    content_checksum TEXT NOT NULL,
    object_key TEXT NOT NULL,
    size_bytes BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- Read a run's checkpoints newest-first (recovery inspects the latest durable boundary, T4).
CREATE INDEX IF NOT EXISTS checkpoints_by_run ON checkpoints (run_id, created_at DESC);

-- The new tables are DML-granted to the application role explicitly (000001's blanket GRANT covered
-- only the tables that existed then). UPDATE is then REVOKED so a checkpoint / transcript boundary is
-- immutable once written (spec §26.1). The REVOKE is load-bearing, not decoration: 000001's
-- `GRANT ... ON ALL TABLES IN SCHEMA public` re-runs on every Migrate and would re-grant UPDATE on
-- these tables once they exist, so — exactly like audit_events (§50.3) — the REVOKE runs after it
-- (000015 follows 000001 in the chain) and keeps UPDATE withheld. DELETE stays for retention/orphan-GC
-- to reclaim expired bytes (T3).
GRANT SELECT, INSERT, DELETE ON transcript_boundaries, checkpoints TO palai_app;
REVOKE UPDATE ON transcript_boundaries, checkpoints FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (15) ON CONFLICT DO NOTHING;
