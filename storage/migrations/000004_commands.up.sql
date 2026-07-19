-- Durable commands (spec §22.4, §9.2). A command is a durable resource with a
-- caller-supplied command_id: its own uniqueness makes a duplicate submission return the
-- original result (NOT the idempotency_records path — commands carry their own idempotency).
-- The lifecycle (queued -> applied | rejected | expired) is the packages/state-machines
-- CommandTable, driven by the coordinator; the journal carries command.accepted/applied/
-- rejected.v1 so an attached client sees the same ordered effects (spec §21.1).
--
-- CREATE TABLE / INDEX ... IF NOT EXISTS keep the migration safe to re-run (Migrate is
-- idempotent, per-boot). The version marker matches the 000002/000003 pattern.

CREATE TABLE IF NOT EXISTS commands (
    -- command_id is caller-supplied and only unique within a tenant, so the tenant scope is
    -- part of the primary key: two tenants may reuse a command_id string as distinct commands.
    id TEXT NOT NULL,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    session_id TEXT NOT NULL REFERENCES sessions (id),
    -- The active root run resolved at accept time. NULL once no run is live (the command was
    -- rejected at accept); a queued send_message always carries the run the pump delivers to.
    run_id TEXT REFERENCES runs (id),
    kind TEXT NOT NULL,
    -- send_message delivery mode (queue | steer | interrupt); NULL for approve/deny.
    delivery TEXT,
    -- The command's own content — e.g. the send_message text (customer content). The journal
    -- events carry only {command_id, kind, delivery, ...}, never the raw text, so a command
    -- event stays metadata.
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    state TEXT NOT NULL DEFAULT 'queued',
    -- The journal sequence where the command took effect (spec §22.4). Set on apply.
    applied_sequence BIGINT,
    -- The terminal result rendered back to the caller (e.g. the rejection reason).
    result JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (organization_id, project_id, id),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The command pump reads a run's queued commands in creation order at each safe boundary.
-- A partial index keeps that tail read off the applied/rejected history.
CREATE INDEX IF NOT EXISTS commands_run_pending_idx
    ON commands (run_id, created_at)
    WHERE state = 'queued';

INSERT INTO schema_migrations (version) VALUES (4) ON CONFLICT DO NOTHING;
