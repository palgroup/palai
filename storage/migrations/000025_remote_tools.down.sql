-- Reverse 000025: drop the remote-tool operation ledger. DROP TABLE IF EXISTS drops its indexes with it
-- and stays idempotent even after an earlier migration dropped a carrier table (the 000024 pattern).
DROP TABLE IF EXISTS remote_tool_operations;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 25;
    END IF;
END
$$;
