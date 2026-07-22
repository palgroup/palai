-- Reverse 000029: drop every tenant_isolation policy and disable row level security, returning
-- isolation to the application's WHERE clauses. Guarded per table so the reversal stays idempotent
-- even after a later DROP removed the carrier (the 000028 pattern).
DO $$
DECLARE
    entry RECORD;
BEGIN
    FOR entry IN
        SELECT c.relname AS table_name
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r' AND c.relrowsecurity
    LOOP
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.table_name);
        EXECUTE format('ALTER TABLE %I NO FORCE ROW LEVEL SECURITY', entry.table_name);
        EXECUTE format('ALTER TABLE %I DISABLE ROW LEVEL SECURITY', entry.table_name);
    END LOOP;
END
$$;

DROP PROCEDURE IF EXISTS palai_apply_tenant_policy(TEXT, TEXT, BOOLEAN);
DROP FUNCTION IF EXISTS palai_tenant_policy_expression(TEXT, BOOLEAN);

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 29;
    END IF;
END
$$;
