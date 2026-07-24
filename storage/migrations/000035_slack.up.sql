-- 000035 adds the Slack integration store (E17 Task 1, spec §36, SLK-001..008). Two tenant-scoped tables:
--   * slack_connections     — an admin-registered Slack workspace binding. The signing secret and bot token
--                             are secret_ref HANDLES only (values live in secret_refs, 000031) — never inline,
--                             so a credential cannot land in a row, a log, or an evidence bundle.
--   * slack_thread_sessions — the (team, channel, thread_ts) -> canonical session correlation. Two events in
--                             the SAME thread resolve the SAME session (SLK-003), and a web-console attach
--                             joins it; last_bot_message_ts is the message-ts reconciliation handle the
--                             rate-limited live-output repair edits (SLK-006).
--
-- Both carry organization_id + project_id, so per the M3 rule (storage/migrations/000030_api_key_scope.up.sql)
-- each asserts its OWN org/project tenant policy in THIS migration — ENABLE + FORCE row level security via
-- palai_apply_tenant_policy — born secured, and tests/security/tenancy gates it. They are the FIRST tables
-- created after 000029's blanket `GRANT ... ON ALL TABLES`, so each needs its own grant or the runtime role
-- fails closed with "permission denied". Both are mutable (a connection is disabled; a thread row's
-- last_bot_message_ts is reconciled), so the full DML grant is correct and needs no self-re-asserting REVOKE.

CREATE TABLE IF NOT EXISTS slack_connections (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    team_id TEXT NOT NULL,                              -- Slack workspace/team id (T...)
    enterprise_id TEXT NOT NULL DEFAULT '',             -- Enterprise Grid org id ('' when not on a grid)
    bot_user_id TEXT NOT NULL DEFAULT '',               -- the app's own bot user id — the self-loop guard (SLK-008)
    signing_secret_ref TEXT NOT NULL,                   -- handle into secret_refs; the v0 verify resolves it
    bot_token_ref TEXT NOT NULL DEFAULT '',             -- handle into secret_refs; outbound chat.* resolves it
    app_token_ref TEXT NOT NULL DEFAULT '',             -- handle into secret_refs; the Socket Mode WS app-token (xapp-) resolves it at connect (T11)
    scopes TEXT NOT NULL DEFAULT '',                    -- granted OAuth scopes (space-separated), advisory
    allowed_channels JSONB NOT NULL DEFAULT '[]'::jsonb, -- allowlist; empty = no channel restriction
    allowed_users JSONB NOT NULL DEFAULT '[]'::jsonb,    -- users permitted to approve/steer (SLK-004)
    default_policy JSONB NOT NULL DEFAULT '{}'::jsonb,   -- default run policy for events on this connection
    disabled BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- One connection per workspace within a project (an enterprise install disambiguates on enterprise_id).
    UNIQUE (organization_id, project_id, team_id, enterprise_id)
);

CALL palai_apply_tenant_policy('slack_connections', 'organization_id', true);
GRANT SELECT, INSERT, UPDATE, DELETE ON slack_connections TO palai_app;

CREATE TABLE IF NOT EXISTS slack_thread_sessions (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    connection_id TEXT NOT NULL REFERENCES slack_connections (id),
    team_id TEXT NOT NULL,
    channel_id TEXT NOT NULL,
    thread_ts TEXT NOT NULL,                             -- the thread root ts — the correlation key
    session_id TEXT NOT NULL REFERENCES sessions (id),
    last_bot_message_ts TEXT NOT NULL DEFAULT '',        -- the visible message the one-shot live-output repair edits
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- One session per (workspace, channel, thread): a second event in the thread resolves the SAME session,
    -- and a concurrent race collapses at the DB (23505 -> the existing row is reused), not two sessions.
    UNIQUE (organization_id, project_id, team_id, channel_id, thread_ts)
);

CALL palai_apply_tenant_policy('slack_thread_sessions', 'organization_id', true);
GRANT SELECT, INSERT, UPDATE, DELETE ON slack_thread_sessions TO palai_app;

INSERT INTO schema_migrations (version) VALUES (35) ON CONFLICT DO NOTHING;
