-- 000048 (E29 session list): the Sessions SCREEN. A session row carried six fields — id, object,
-- status, created_at, organization_id, project_id — and a list screen needs a label, the agent, the
-- tokens and the duration, none of which existed on the row. This migration adds the ONE thing that is
-- genuinely absent (a label) and the FOUR indexes without which the other three, which are aggregates
-- over rows that already exist, would each turn a 50-row page into a sequential scan.
--
-- WHAT IS NOT HERE, and each absence is a decision:
--
--   - No session→agent column. The association is per-RUN and always has been (000019 put
--     agent_revision_id on `runs`), so a session has AGENTS, plural, and they are an aggregate. Adding
--     a session-level column would invent an association the executor never writes.
--   - No token counters on sessions. usage_ledger (000032) already has a single writer that settles
--     every model step exactly once by dedupe key. A denormalised counter would be a SECOND writer for
--     a number that already has one, and the two would diverge on exactly the paths the ledger was
--     built to survive: redelivery, replay, and a rolled-back attempt.
--   - No started_at/ended_at on sessions. `runs` already records both ends: created_at is written by
--     InsertRun and updated_at by UpdateRunState on every transition
--     (storage/queries/responses.sql). A span copied onto the session would be a third writer of a
--     fact two columns already carry.

-- ---------------------------------------------------------------------------------------------------
-- (1) sessions.name — the operator's LABEL.
-- ---------------------------------------------------------------------------------------------------
--
-- DELIBERATELY NOT UNIQUE. The reference screen shows several sessions sharing one label, so this is a
-- name in the human sense and not an identifier; a unique index here would reject a legitimate rename.
-- The id remains the identity.
--
-- TEXT NOT NULL DEFAULT '' rather than a NULLable column, for 000046's stated reason: every existing
-- session in every deployment has no label, and '' is one kind of nothing where NULL plus '' would be
-- two. The API distinguishes "the operator named it" from "we derived one" with an explicit
-- name_source field rather than by overloading emptiness.
--
-- It lands in the same change as the code that reads it (the session projection) and the routes that
-- write it (POST /v1/sessions body, PATCH /v1/sessions/{id}), which is the rule 000019 wrote for itself
-- and 000046 obeyed: a column nothing reads must not be stored.
ALTER TABLE sessions ADD COLUMN IF NOT EXISTS name TEXT NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------------------------------
-- (2) The four indexes. Three serve the new aggregates; one repairs a list that never had its own.
-- ---------------------------------------------------------------------------------------------------
--
-- sessions_tenant_keyset_idx is NOT part of this feature — it is a repair, and it is the reason the
-- feature is affordable. ListSessions has ordered by (created_at DESC, id DESC) under an
-- (organization_id, project_id) predicate since E13 T4, and `sessions` has carried exactly one index
-- the whole time: its primary key on `id`. Measured on the local stack's own database, 2026-07-31:
--
--   EXPLAIN (ANALYZE, BUFFERS, COSTS OFF)
--     SELECT id, state, created_at FROM sessions
--     WHERE organization_id = 'org_local' AND project_id = 'prj_local'
--     ORDER BY created_at DESC, id DESC LIMIT 21;
--   -> Limit -> Sort (Sort Key: created_at DESC, id DESC) -> Seq Scan on sessions
--
-- Every page of every session list sorted the whole table. The column order matches the query exactly:
-- equality on the tenant pair, then the keyset in its own direction, so the LIMIT is satisfied by
-- walking the index and the Sort disappears.
DO $$
BEGIN
    -- GUARDED BY A.2 TASK 6 (see 000067): the chain re-applies IN FULL on every boot, and 000067 drops
    -- organization_id. A bare `CREATE INDEX IF NOT EXISTS` still RESOLVES its column list even when the
    -- index already exists, so the second boot would fail here with 42703. 000065 already rebuilt this
    -- index project-keyed under the SAME name; the statement below is the fresh-install path only.
    IF EXISTS (SELECT 1 FROM pg_attribute att
                 JOIN pg_class cls ON cls.oid = att.attrelid
                 JOIN pg_namespace ns ON ns.oid = cls.relnamespace
                WHERE ns.nspname = 'public' AND cls.relname = 'sessions'
                  AND att.attname = 'organization_id' AND att.attnum > 0 AND NOT att.attisdropped) THEN
        CREATE INDEX IF NOT EXISTS sessions_tenant_keyset_idx
            ON sessions (organization_id, project_id, created_at DESC, id DESC);
    END IF;
END
$$;

-- responses_session_created_idx serves the DERIVED label (the first non-retracted response of a
-- session, in (created_at, id) order) — and, not incidentally, SessionHistory, which reads exactly this
-- shape (`WHERE session_id = $1 ... ORDER BY created_at, id`) on the way into EVERY model step and had
-- no index either. `responses` also carried only its primary key.
CREATE INDEX IF NOT EXISTS responses_session_created_idx
    ON responses (session_id, created_at, id);

-- runs_session_created_idx serves BOTH the agent aggregate and the activity span, which read the
-- session's runs once between them. The existing runs_one_active_root_per_session index cannot: it is
-- PARTIAL over the non-terminal states, so it excludes precisely the finished runs a completed
-- session's span and agent list are made of.
CREATE INDEX IF NOT EXISTS runs_session_created_idx
    ON runs (session_id, created_at);

-- usage_ledger_session_idx serves the token aggregate. The two existing indexes are keyed on meter and
-- on the ledger's own keyset; neither can find one session's rows. The tenant pair leads because the
-- aggregate filters on it EXPLICITLY: 000032 secures usage_ledger at the ORGANIZATION level with
-- has_project=false, so the project narrowing is the query's job, not RLS's, and an aggregate that
-- omitted it would silently sum every sibling project's tokens into this session's row.
DO $$
BEGIN
    -- GUARDED BY A.2 TASK 6 (see 000067): the chain re-applies IN FULL on every boot, and 000067 drops
    -- organization_id. A bare `CREATE INDEX IF NOT EXISTS` still RESOLVES its column list even when the
    -- index already exists, so the second boot would fail here with 42703. 000065 already rebuilt this
    -- index project-keyed under the SAME name; the statement below is the fresh-install path only.
    IF EXISTS (SELECT 1 FROM pg_attribute att
                 JOIN pg_class cls ON cls.oid = att.attrelid
                 JOIN pg_namespace ns ON ns.oid = cls.relnamespace
                WHERE ns.nspname = 'public' AND cls.relname = 'usage_ledger'
                  AND att.attname = 'organization_id' AND att.attnum > 0 AND NOT att.attisdropped) THEN
        CREATE INDEX IF NOT EXISTS usage_ledger_session_idx
            ON usage_ledger (organization_id, project_id, session_id, meter);
    END IF;
END
$$;

-- No grant and no policy call: `sessions` is a 000001 table already covered by the blanket GRANT and by
-- 000029's tenant policy, and a new COLUMN inherits both. The four indexes are objects on existing
-- tables and need neither.

INSERT INTO schema_migrations (version) VALUES (48) ON CONFLICT DO NOTHING;
