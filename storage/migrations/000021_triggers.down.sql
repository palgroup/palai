-- Reverse 000021: drop the trigger tables (children before parents — trigger_deliveries references
-- trigger_revisions references triggers). DROP ... IF EXISTS keeps the rollback idempotent even after an
-- earlier migration has already removed a dependency (the 000019/000020 pattern).

DROP TABLE IF EXISTS trigger_deliveries;
DROP TABLE IF EXISTS trigger_revisions;
DROP TABLE IF EXISTS triggers;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 21;
    END IF;
END
$$;
