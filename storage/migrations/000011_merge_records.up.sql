-- Explicit child->parent merge record (spec §30.5, REP-011). A merge of a child agent's isolated
-- branch into the parent worktree is an EXPLICIT operation that detects conflicts and RECORDS the
-- source child run — a conflict is reported, never silently overwritten. This table is that durable
-- record: which child run's branch was merged into which parent run, whether it merged or conflicted,
-- and the conflicting paths.
--
-- No separate worktree table: a child run already carries its parent_run_id (000007) and its
-- workspace_mode in the delegation column (§25.18), and the child branch is the derived
-- agent/<session-short>/<run-short> (§30.5). The worktree itself is ephemeral filesystem state, not
-- durable domain state — recording/restoring it is E10 recovery's concern.
--
-- CREATE ... IF NOT EXISTS keeps the migration idempotent (000008/000009 pattern); tenant scope is
-- the composite (organization_id, project_id) FK; the table is granted to palai_app explicitly.

CREATE TABLE IF NOT EXISTS merge_records (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The parent run whose worktree received the merge, and the child run whose branch was merged
    -- (spec §30.5 "records source child run", REP-011).
    parent_run_id TEXT NOT NULL REFERENCES runs (id),
    source_child_run_id TEXT NOT NULL REFERENCES runs (id),
    child_branch TEXT NOT NULL,
    -- The outcome: merged=true with the resulting commit, or merged=false with the conflicting paths.
    -- A conflict leaves the parent worktree consistent (the merge was aborted), so a false row is the
    -- explicit-resolution signal, not a half-applied state.
    merged BOOLEAN NOT NULL,
    merge_commit TEXT NOT NULL DEFAULT '',
    conflict_paths JSONB NOT NULL DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

GRANT SELECT, INSERT, UPDATE, DELETE ON merge_records TO palai_app;

INSERT INTO schema_migrations (version) VALUES (11) ON CONFLICT DO NOTHING;
