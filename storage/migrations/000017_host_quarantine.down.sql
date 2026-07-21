-- Reverse of 000017_host_quarantine.up.sql. Drops the quarantine table + the merge_records index, and
-- reverts the workspace_snapshots rider columns, before 000008 drops workspace_snapshots itself. DROP ...
-- IF EXISTS keeps the rollback idempotent even after an earlier migration has already removed the
-- referenced tables (the 000015/000016 pattern).

DROP INDEX IF EXISTS merge_records_by_parent_run;

ALTER TABLE IF EXISTS workspace_snapshots DROP COLUMN IF EXISTS object_key;
ALTER TABLE IF EXISTS workspace_snapshots DROP COLUMN IF EXISTS archive_checksum;
ALTER TABLE IF EXISTS workspace_snapshots DROP COLUMN IF EXISTS size_bytes;

DROP TABLE IF EXISTS host_quarantine;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 17;
    END IF;
END
$$;
