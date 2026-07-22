-- Reverse 000028: drop the hooks table (and its index with it). DROP ... IF EXISTS keeps the rollback
-- idempotent even after an earlier migration dropped the carrier (the 000024/000026 pattern).
DROP TABLE IF EXISTS hooks;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 28;
    END IF;
END
$$;
