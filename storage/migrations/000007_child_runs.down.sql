-- Reverse of 000007_child_runs.up.sql: drops the child-run columns and the one-active-root
-- index. The index goes before parent_run_id (its WHERE references the column). The
-- child-EXCLUDING predicate cannot be reverted to the 000006 root-inclusive one while child
-- rows exist (rebuilding the broad index over a session that already has a child would be the
-- very unique_violation the predicate change avoids), so the reversal simply drops the index —
-- the full down chain's 000006 reversal owned it and 000001 drops the table; a re-Migrate
-- recreates the new-predicate index over the (empty) table. Guarded so the reversal stays
-- idempotent even after 000001 has dropped the runs table.

DROP INDEX IF EXISTS runs_one_active_root_per_session;

DO $$
BEGIN
    IF to_regclass('public.runs') IS NOT NULL THEN
        ALTER TABLE runs DROP COLUMN IF EXISTS delegation;
        ALTER TABLE runs DROP COLUMN IF EXISTS depth;
        ALTER TABLE runs DROP COLUMN IF EXISTS parent_run_id;
    END IF;
END
$$;

DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 7;
    END IF;
END
$$;
