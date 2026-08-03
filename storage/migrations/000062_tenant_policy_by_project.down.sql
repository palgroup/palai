-- Reverse 000062: restore 000029's organization_id-keyed policy expression and re-apply it to every
-- table this migration rekeyed to project_id, so a downgrade actually undoes what the up migration did
-- rather than leaving the project-keyed expression installed underneath an untested rollback. budgets
-- and quotas restore their 000032 has_project=false form; every other swept table restores has_project
-- =true, which was 000029's own default for any table carrying project_id alongside organization_id.
-- api_keys, principals and usage_ledger were never touched by 000062's sweep (see its header), so they
-- need no restoration here — their 000030/000032 policies were never replaced.
CREATE OR REPLACE FUNCTION palai_tenant_policy_expression(tenant_key TEXT, has_project BOOLEAN)
RETURNS TEXT
LANGUAGE sql IMMUTABLE
AS $$
    SELECT format(
        'coalesce(current_setting(''palai.system'', true), '''') = ''on'' OR (%s = current_setting(''palai.org_id'', true)%s)',
        tenant_key,
        CASE WHEN has_project THEN
            ' AND (coalesce(current_setting(''palai.project_id'', true), '''') = '''' OR project_id = current_setting(''palai.project_id'', true))'
        ELSE '' END);
$$;

CALL palai_apply_tenant_policy('budgets', 'organization_id', false);
CALL palai_apply_tenant_policy('quotas', 'organization_id', false);

DO $$
DECLARE entry RECORD;
BEGIN
    FOR entry IN
        SELECT c.relname AS table_name
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public' AND c.relkind = 'r'
           AND c.relname NOT IN ('budgets', 'quotas', 'api_keys', 'principals', 'usage_ledger')
           AND EXISTS (SELECT 1 FROM information_schema.columns col
                        WHERE col.table_schema = 'public' AND col.table_name = c.relname
                          AND col.column_name = 'project_id')
    LOOP
        CALL palai_apply_tenant_policy(entry.table_name, 'organization_id', true);
    END LOOP;
END
$$;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations (the 000029
-- down-migration precedent).
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 62;
    END IF;
END
$$;
