-- Slack connection management + inbound resolution + thread↔session correlation (spec §36, E17 Task 1,
-- SLK-001..008). Create/read/list are the admin management surface (tenant-scoped by organization_id +
-- project_id). ResolveSlackConnectionByTeam is the UNAUTHENTICATED inbound path's tenant establisher (the
-- resolveInboundTrigger idiom): it is keyed by the Slack team id the callback carries and runs system-scoped
-- because there is no tenant yet — the caller still has to present a valid v0 signature over the resolved
-- connection's signing secret before anything is written. The thread queries collapse a (team, channel,
-- thread) to one canonical session.

-- name: InsertSlackConnection
INSERT INTO slack_connections (
    id, organization_id, project_id, team_id, enterprise_id, bot_user_id,
    signing_secret_ref, bot_token_ref, scopes, allowed_channels, allowed_users, default_policy)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12);

-- GetSlackConnection reads a connection's metadata within scope. The secret refs are HANDLES, not values,
-- so they are safe to return to an admin read; the resolved bytes never live in this table.
-- name: GetSlackConnection
SELECT id, team_id, enterprise_id, bot_user_id, signing_secret_ref, bot_token_ref, scopes, disabled
FROM slack_connections
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListSlackConnections pages a project's connections newest-first (the admin ListView envelope). Tenant
-- scoped by RLS; the org/project predicate is defence-in-depth. The secret refs are omitted from a list.
-- name: ListSlackConnections
SELECT id, team_id, enterprise_id, bot_user_id, disabled, created_at
FROM slack_connections
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- ResolveSlackConnectionByTeam establishes the tenant for a signed inbound callback, keyed by the Slack
-- team + enterprise id. System-scoped (there is no tenant to scope by yet); the signature over the returned
-- signing_secret_ref is the auth. A disabled connection still resolves so the caller can reject explicitly.
-- name: ResolveSlackConnectionByTeam
SELECT id, organization_id, project_id, signing_secret_ref, bot_token_ref, bot_user_id, disabled
FROM slack_connections
WHERE team_id = $1 AND enterprise_id = $2;

-- CorrelateThreadSession claims the (team, channel, thread) -> session mapping single-winner. A first event
-- inserts its session; a later event in the SAME thread hits the unique index (23505) and inserts nothing,
-- so the caller reads the canonical session with GetThreadSession — one session per thread (SLK-003), race
-- included. RETURNING id lets the caller tell a fresh claim from a reuse.
-- name: CorrelateThreadSession
INSERT INTO slack_thread_sessions (
    id, organization_id, project_id, connection_id, team_id, channel_id, thread_ts, session_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
ON CONFLICT (organization_id, project_id, team_id, channel_id, thread_ts) DO NOTHING
RETURNING id;

-- GetThreadSession reads the canonical session a (team, channel, thread) already resolved to — the reuse
-- path (a second thread event, or a web-console attach joining the SAME session).
-- name: GetThreadSession
SELECT session_id, last_bot_message_ts
FROM slack_thread_sessions
WHERE organization_id = $1 AND project_id = $2 AND team_id = $3 AND channel_id = $4 AND thread_ts = $5;

-- UpdateThreadMessageTS records the visible bot message ts the rate-limited live-output repair edits
-- (message-ts reconciliation, SLK-006). Idempotent — it just overwrites the handle.
-- name: UpdateThreadMessageTS
UPDATE slack_thread_sessions
SET last_bot_message_ts = $6
WHERE organization_id = $1 AND project_id = $2 AND team_id = $3 AND channel_id = $4 AND thread_ts = $5;
