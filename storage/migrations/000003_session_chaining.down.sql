-- Reverse of 000003_session_chaining.up.sql. Drops the added index and columns before
-- 000001 drops the tables that carried them, so an up -> down -> up reapply is clean.
-- ALTER/DROP ... IF EXISTS keeps the rollback idempotent even after 000001 has removed
-- the tables.

DROP INDEX IF EXISTS events_response_id_idx;

ALTER TABLE IF EXISTS events
    DROP COLUMN IF EXISTS response_id;

ALTER TABLE IF EXISTS sessions
    DROP COLUMN IF EXISTS active_root_run_id;

-- Guarded so the rollback stays idempotent even after 000001 has dropped the table.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 3;
    END IF;
END
$$;
