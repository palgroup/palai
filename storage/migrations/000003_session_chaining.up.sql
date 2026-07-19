-- Session chaining (spec §9, §21.1, §22.2). Multi-response sessions need two things
-- the LP-0 one-shot schema did not: a way to name a session's live root run, and a way
-- to key journal events per response so a store:false purge of one response never scrubs
-- a retained sibling's journal (the 000002 scrub ceiling — storage/queries/responses.sql).
--
-- Only ALTERs on tables created by 000001, so no new grants are needed: the palai_app
-- privileges already cover the new columns. Every ADD COLUMN / CREATE INDEX IF NOT EXISTS
-- keeps the migration safe to re-run (Migrate is idempotent).

ALTER TABLE sessions
    -- Names the session's non-terminal root run. Prep for T4's one-active-root partial
    -- unique constraint (§22.3); this task only adds the column, T4 wires the invariant.
    ADD COLUMN IF NOT EXISTS active_root_run_id TEXT;

ALTER TABLE events
    -- Run-scoped events carry the owning response; session-scoped events stay NULL (they
    -- hold no customer content, so the retention purge leaves them alone). Nullable so the
    -- 000001 rows and future session-scoped events are both valid (spec §22.2).
    ADD COLUMN IF NOT EXISTS response_id TEXT;

-- The retention scrub joins events by response_id; index it so the per-response purge
-- does not table-scan the journal.
CREATE INDEX IF NOT EXISTS events_response_id_idx ON events (response_id);

-- Backfill the upgrade boundary: events written before this migration carry a NULL
-- response_id the per-response scrub cannot reach, so a store:false response admitted
-- pre-000003 and purged after would keep its content. LP-0 is 1:1 session:response, so each
-- session's events map unambiguously to its sole response. Gated on the version marker so it
-- runs ONCE on first apply — Migrate runs per boot, and a re-run must not re-key the
-- session-scoped events T2 introduces, which are designed to stay NULL (plan §4).
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 3) THEN
        UPDATE events e
        SET response_id = r.id
        FROM responses r
        WHERE r.session_id = e.session_id AND e.response_id IS NULL;
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT DO NOTHING;
