-- Reverse of 000011_merge_records.up.sql. Drops the table before 000001 drops the runs/projects it
-- keys to. DROP ... IF EXISTS keeps the rollback idempotent even after 000001 has already removed
-- the referenced tables (the 000008/000009 pattern).

DROP TABLE IF EXISTS merge_records;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 11;
    END IF;
END
$$;
