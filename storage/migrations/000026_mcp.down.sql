-- Reverse 000026: drop the mcp_connections table. Discovered tool_revisions rows (executor='mcp') are
-- 000024 objects and are dropped by 000024's down migration, not here. DROP ... IF EXISTS keeps the
-- rollback idempotent even after an earlier migration dropped the carrier (the 000024 pattern).
DROP TABLE IF EXISTS mcp_connections;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 26;
    END IF;
END
$$;
