-- Reverse 000002: drop every tenant_isolation policy and turn row-level security off, returning isolation
-- to the application's WHERE clauses. The sweep reads pg_class rather than a written-out list, so it stays
-- correct when a table has already gone.

DO $$
DECLARE entry RECORD;
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

DROP PROCEDURE IF EXISTS palai_apply_child_policy(TEXT, TEXT, TEXT);
DROP PROCEDURE IF EXISTS palai_apply_installation_policy(TEXT);
DROP PROCEDURE IF EXISTS palai_apply_tenant_policy(TEXT, TEXT);
DROP FUNCTION IF EXISTS palai_installation_policy_expression();
DROP FUNCTION IF EXISTS palai_tenant_policy_expression(TEXT);

-- Guarded: the down chain runs 000002 before 000001, but a re-run reaches this after 000001 has already
-- dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 2;
    END IF;
END
$$;
