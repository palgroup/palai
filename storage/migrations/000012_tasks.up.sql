-- 000012_tasks: the durable, session-scoped task/todo registry (spec §11, master plan line 410).
-- These are the model-facing durable primitives behind the task/todo tools: DB-backed and
-- SESSION-scoped (they outlive a run, so a context-reset attempt reads what is done and what is not
-- straight from here — REG-001), multi-client visible through the ordered event journal (REG-002).
-- One table with a `kind` discriminator carries both tasks and todos — they are the same shape.

CREATE TABLE IF NOT EXISTS tasks (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    task_key TEXT NOT NULL,                          -- the model-chosen stable key within the session
    kind TEXT NOT NULL DEFAULT 'task',               -- 'task' | 'todo'
    title TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'open',             -- open | in_progress | done | canceled (free text; the tool guides)
    detail JSONB NOT NULL DEFAULT '{}'::jsonb,        -- arbitrary model metadata
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (session_id, task_key)                    -- one row per (session, key): upsert target, idempotent
);

-- Session-scoped list reads order by creation time; the index serves both the read and the upsert
-- conflict lookup.
CREATE INDEX IF NOT EXISTS tasks_session_created_idx ON tasks (session_id, created_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON tasks TO palai_app;

INSERT INTO schema_migrations (version) VALUES (12) ON CONFLICT DO NOTHING;
