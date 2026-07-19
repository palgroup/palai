-- Reverse of 000006_one_active_root.up.sql. Drops the one-active-root index before 000001
-- drops the runs table that carries it. DROP INDEX IF EXISTS keeps the rollback idempotent
-- even after 000001 has already removed the table.

DROP INDEX IF EXISTS runs_one_active_root_per_session;

-- Guarded so the rollback stays idempotent even after 000001 has dropped the table.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 6;
    END IF;
END
$$;
