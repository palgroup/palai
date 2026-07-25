-- Append-only security audit (spec §50.3). The event stream records a denied attach
-- here when a caller reaches for a session outside its verified scope. The row is keyed
-- to the ACTOR's tenant and names only the caller-supplied id, so a cross-tenant session
-- and an unknown one produce the identical content-free denial — no existence is disclosed
-- (spec §39.2). detail defaults to '{}'; palai_app may INSERT but never UPDATE/DELETE.

-- name: InsertAttachDenial
INSERT INTO audit_events (organization_id, project_id, actor, action, outcome, resource)
VALUES ($1, $2, $3, 'session.attach', 'denied', $4);

-- AuditChainRows reads the WHOLE `events` journal in canonical chain order for the SEC-103 integrity
-- chain (E18 T7). Every column of the table is selected: a column left out of the SELECT is a column
-- left out of the digest, i.e. one an attacker could edit without raising a tamper alert
-- (packages/audit.ChainedColumns is guard-tested against the table's real column set).
--
-- The two renderings exist so the digest does not depend on the CLIENT: payload::text is Postgres's
-- canonical jsonb form (key order normalized by the server, not by Go's map iteration), and created_at
-- is pinned to UTC microseconds rather than the connection's TimeZone setting.
-- name: AuditChainRows
SELECT id,
       organization_id,
       project_id,
       session_id,
       response_id,
       seq,
       journal_id,
       type,
       payload::text,
       to_char(created_at AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')
FROM events
ORDER BY session_id, seq;
