-- Session config revisions (spec §9.3, §14). A change_config command creates a content-
-- addressed session config revision that applies at the next model-step boundary: the
-- orchestrator reads the latest revision when it routes a step, so a normal switch takes the
-- NEXT step and an immediate switch takes the step after the interrupt boundary. The provider
-- never changes here (E06 §7.3 carve-out) — only the model id and the tool set move.
--
-- Every ADD COLUMN / CREATE TABLE / CREATE INDEX ... IF NOT EXISTS keeps the migration safe to
-- re-run (Migrate is idempotent, per-boot). Only ALTERs on 000001 tables and one new table, so
-- the palai_app grants already cover it. The version marker matches the 000002..000004 pattern.

ALTER TABLE projects
    -- The project's config policy: {allowed_models, allowed_tools, default_tools}. NULL means
    -- unrestricted (every existing project), so a config change is denied only against a project
    -- that declares an allowlist (spec §9.3 typed denial, §14.4 project baseline). allowed_*
    -- constrain a change; default_tools is the tools baseline the snapshot resolves from.
    ADD COLUMN IF NOT EXISTS config_policy JSONB;

CREATE TABLE IF NOT EXISTS config_revisions (
    id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    -- The change_config command that created this revision (its applied_sequence is `sequence`).
    command_id TEXT,
    -- The journal sequence where the revision took effect — the safe boundary it applied at
    -- (spec §9.3, §22.4). The latest revision by this sequence is the session's effective config.
    sequence BIGINT NOT NULL,
    -- The cumulative effective session override this revision resolved to. Empty model / NULL
    -- tools mean "inherit the lower layer"; the orchestrator routes the model id here per step.
    model TEXT NOT NULL DEFAULT '',
    tools JSONB,
    -- The content address of the effective ConfigSnapshot (SHA-256 over canonical JSON). The
    -- redacted snapshot with provenance rides the config.revised.v1 journal event, not this row.
    snapshot_hash TEXT NOT NULL,
    -- true for an immediate switch (the in-flight step was interrupted before this applied).
    immediate BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The orchestrator reads a session's latest revision at each model step; index the tail so that
-- per-step read never scans a session's revision history.
CREATE INDEX IF NOT EXISTS config_revisions_session_seq_idx
    ON config_revisions (session_id, sequence DESC);

INSERT INTO schema_migrations (version) VALUES (5) ON CONFLICT DO NOTHING;
