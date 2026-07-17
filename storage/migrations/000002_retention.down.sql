-- Reverse of 000002_retention.up.sql. Drops the retention columns before 000001
-- drops the tables, so an up -> down -> up reapply is clean. ALTER TABLE IF EXISTS
-- keeps the rollback safe even after 000001 has already removed the tables.

ALTER TABLE IF EXISTS idempotency_records
    DROP COLUMN IF EXISTS outcome_fingerprint,
    DROP COLUMN IF EXISTS resource_tombstone,
    DROP COLUMN IF EXISTS result_purged_at;

ALTER TABLE IF EXISTS responses
    DROP COLUMN IF EXISTS purged_at,
    DROP COLUMN IF EXISTS store;

-- Guarded so the rollback stays idempotent even after 000001 has dropped the table.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 2;
    END IF;
END
$$;
