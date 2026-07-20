-- Reverse of 000009_repository_bindings.up.sql. Drops the two tables in reverse dependency order
-- (preparation_receipts references repository_bindings) before 000001 drops the projects/runs they
-- key to. DROP ... IF EXISTS keeps the rollback idempotent even after 000001 has already removed
-- the referenced tables (the 000005/000008 pattern).

DROP TABLE IF EXISTS preparation_receipts;
DROP TABLE IF EXISTS repository_bindings;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 9;
    END IF;
END
$$;
