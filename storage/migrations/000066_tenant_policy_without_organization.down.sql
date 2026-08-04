-- 000066 down. It restores the organization-keyed policies on the seven tables this migration rekeyed and
-- drops the installation-policy helpers it introduced, so a rollback leaves no expression behind that a
-- re-applied 000029/000062 would not re-assert.
--
-- The four child tables go back to 000029's parent-organization EXISTS form; the three identity tables go
-- back to 000029's organization_id/id form, which is what 000062 deliberately left them with. That is the
-- state 000062's own down file expects to find, and it is reachable: unlike 000065's 111 objects, these are
-- seven policies whose previous text is written down in two migrations that are still in this chain.
--
-- palai_tenant_policy_expression is NOT restored here. It is 000062's function, 000062's down owns it, and
-- reversing it from this file would leave two migrations writing the same definition.

DO $$
DECLARE entry RECORD;
DECLARE expression TEXT;
BEGIN
    IF to_regclass('public.organizations') IS NULL THEN
        RETURN;
    END IF;
    -- The three identity tables, keyed as 000029 keyed them.
    FOR entry IN
        SELECT * FROM (VALUES
            ('api_keys', 'organization_id'),
            ('principals', 'organization_id'),
            ('projects', 'organization_id')
        ) AS t(target, tenant_key)
    LOOP
        expression := format(
            'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
            'OR (%I = current_setting(''palai.org_id'', true))', entry.tenant_key);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.target);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                       entry.target, expression, expression);
    END LOOP;

    -- The four installation-wide tables, back to their own organization_id.
    FOR entry IN
        SELECT * FROM (VALUES
            ('environments', 'organization_id'),
            ('environment_values', 'organization_id'),
            ('secret_refs', 'organization_id'),
            ('usage_ledger', 'organization_id')
        ) AS t(target, tenant_key)
    LOOP
        expression := format(
            'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
            'OR (%I = current_setting(''palai.org_id'', true))', entry.tenant_key);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.target);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                       entry.target, expression, expression);
    END LOOP;

    -- The four children, back to resolving the parent's organization.
    FOR entry IN
        SELECT * FROM (VALUES
            ('delivery_attempts',     'webhook_deliveries', 'delivery_id', 'id'),
            ('job_attempts',          'durable_jobs',       'job_id',      'id'),
            ('model_route_revisions', 'model_routes',       'route_id',    'id'),
            ('schedule_occurrences',  'schedules',          'schedule_id', 'id')
        ) AS t(child, parent, child_key, parent_key)
    LOOP
        expression := format(
            'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
            'OR EXISTS (SELECT 1 FROM %1$I p WHERE p.%2$I = %3$I.%4$I '
            'AND p.organization_id = current_setting(''palai.org_id'', true))',
            entry.parent, entry.parent_key, entry.child, entry.child_key);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.child);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                       entry.child, expression, expression);
    END LOOP;
END
$$;

DROP PROCEDURE IF EXISTS palai_apply_installation_policy(TEXT);
DROP FUNCTION IF EXISTS palai_installation_policy_expression();

DELETE FROM schema_migrations WHERE version = 66;
