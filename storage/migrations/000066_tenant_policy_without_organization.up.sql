-- 000066 (tenant policy without organization, A.2 Task 6): the last twelve row-level-security policies
-- that read `palai.org_id` are rekeyed. After this migration the ONLY one left is `organizations`' own,
-- and 000067 drops that table with it. Nothing stops writing organization_id here — this migration exists
-- so that when the writers stop, no policy is left comparing a NULL to the GUC and quietly hiding rows.
--
-- 000062 moved the boundary from organization to project for every table that carried a project_id and was
-- not one of its three named exclusions. These twelve are what it left, and they are FOUR different
-- situations, not one — the fourth is the one this epic's plan never named:
--
--   SELECT tablename FROM pg_policies WHERE schemaname='public'
--     AND (coalesce(qual,'') LIKE '%palai.org_id%' OR coalesce(with_check,'') LIKE '%palai.org_id%');
--   -- api_keys, delivery_attempts, environment_values, environments, job_attempts,
--   -- model_route_revisions, organizations, principals, projects, schedule_occurrences,
--   -- secret_refs, usage_ledger              (12, measured 2026-08-04, chain applied through 000065)
--
-- (1) THE IDENTITY TABLES, rekeyed to project — api_keys, principals, projects. 000062 excluded the first
--     two by name and quoted 000030's reason: the provisioning API widens to the whole organization
--     (palai.project_id empty) so an org admin manages every project's keys. That reason ENDS with
--     organizations. A key is now administered by the project it belongs to, and the widening a platform
--     operator still needs is a SYSTEM scope, which every policy here admits and which is greppable at the
--     one call site that takes it (identity's provisioningScope). `projects` is keyed on its own `id`,
--     because a project's own row is the project.
--
-- (2) THE FOUR CHILD TABLES whose policy resolved the PARENT's organization through EXISTS —
--     delivery_attempts -> webhook_deliveries, job_attempts -> durable_jobs, model_route_revisions ->
--     model_routes, schedule_occurrences -> schedules. None of them carries a tenant column of its own, so
--     dropping a column could never have fixed them; each EXISTS is rewritten to read the parent's
--     project_id. Every one of the four parents carries one (verified against the catalogue), so this is a
--     rekey and not a widening.
--
-- (3) THE INSTALLATION-WIDE TABLES, and this is the one place in A.2 where removing organizations REMOVES
--     a boundary instead of moving it. environments, environment_values and secret_refs carry NO project_id
--     at all (000046, 000031) — there is nothing narrower than the installation for a policy to key on, and
--     giving them one would be a product change (whose project owns an existing environment?) rather than
--     the removal this epic is. usage_ledger DOES carry project_id and still joins this class deliberately:
--     000062 excluded it because ExhaustedBudget/ExhaustedQuota (storage/queries/usage.sql) sum EVERY
--     sibling project's spend from ONE project-scoped connection against an installation-wide budget, and a
--     project-keyed policy would hide the siblings and UNDERCOUNT — a budget that fails open, silently.
--
--     What they get instead of no policy at all: a policy that still denies a connection which NEVER
--     declared a scope. That keeps deny-by-default — tests/security/tenancy's
--     TestConnectionWithoutTenantContextSeesNoTenantRows drives raw pgx with no GUC set on purpose — and it
--     keeps RLS ENABLED and FORCED on these tables, so they need no entry on that suite's non-tenant
--     allowlist and a later project_id column can be keyed on without first re-enabling anything.
--
--     THE CEILING ON THAT GUARD, measured rather than assumed, because it applies to 000062's identical
--     `IS NOT NULL` too: a custom GUC cannot return to NULL once it has been set in a session. Against the
--     pinned image, `set_config('palai.project_id', NULL, false)` and `RESET palai.project_id` BOTH leave
--     it reading as the empty string. So `current_setting(...) IS NOT NULL` distinguishes a connection
--     that never carried a scope from one that ever did — not "this borrow declared nothing" from "the
--     previous borrow declared something". storage.OpenPool republishes the scope on every acquisition, so
--     in production every app connection satisfies the guard permanently and these four tables are, for
--     it, unconditionally readable. That is what installation-wide means; it is written here so nobody
--     reads the guard as narrower than it is.
--
--     Stated plainly so it is a decision and not a discovery: within ONE installation, every project can
--     read every environment, environment value, secret ref and usage row. That is EXACTLY what is true
--     today (all of them are organization-wide and an installation has one organization) — but it is true
--     today because of a boundary, and after this migration it is true because there is none. Palai Cloud's
--     model is one installation per customer, which is what makes it acceptable; an installation that ever
--     hosts two customers needs project_id on these four tables FIRST.
--
-- (4) organizations — untouched. Its policy compares its own `id` to palai.org_id, which is correct right
--     up to the moment 000067 drops the table, and rekeying a table that is about to be deleted would be
--     work spent to be undone.

-- The installation-wide expression. It is deliberately NOT `true`: a connection that NEVER declared a scope
-- must still see nothing, which is the property 000029 installed and 000062's own `IS NOT NULL` guard
-- preserved for the same reason — read the ceiling above for exactly how much that guard is worth.
CREATE OR REPLACE FUNCTION palai_installation_policy_expression()
RETURNS TEXT
LANGUAGE sql IMMUTABLE
AS $$
    SELECT 'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
           'OR current_setting(''palai.project_id'', true) IS NOT NULL';
$$;

CREATE OR REPLACE PROCEDURE palai_apply_installation_policy(target TEXT)
LANGUAGE plpgsql
AS $$
DECLARE expression TEXT := palai_installation_policy_expression();
BEGIN
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', target);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', target);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', target);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                   target, expression, expression);
END
$$;

-- 1. The identity tables. palai_apply_tenant_policy is 000029's procedure carrying 000062's project-keyed
-- expression, so these three end up with the IDENTICAL rule every other project table already has rather
-- than a second variant written here.
CALL palai_apply_tenant_policy('api_keys', 'project_id', false);
CALL palai_apply_tenant_policy('principals', 'project_id', false);
CALL palai_apply_tenant_policy('projects', 'id', false);

-- 2. The installation-wide tables.
CALL palai_apply_installation_policy('environments');
CALL palai_apply_installation_policy('environment_values');
CALL palai_apply_installation_policy('secret_refs');
CALL palai_apply_installation_policy('usage_ledger');

-- 3. The four children, each reading its parent's project. Written out per table rather than swept, the
-- shape 000029 used for the same four and for the same reason: the parent and the joining column differ
-- every time, so a loop would need a hand-written table of them anyway and would hide it inside a query.
DO $$
DECLARE entry RECORD;
DECLARE expression TEXT;
BEGIN
    FOR entry IN
        SELECT * FROM (VALUES
            ('delivery_attempts',      'webhook_deliveries', 'delivery_id',  'id'),
            ('job_attempts',           'durable_jobs',       'job_id',       'id'),
            ('model_route_revisions',  'model_routes',       'route_id',     'id'),
            ('schedule_occurrences',   'schedules',          'schedule_id',  'id')
        ) AS t(child, parent, child_key, parent_key)
    LOOP
        expression := format(
            'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
            'OR EXISTS (SELECT 1 FROM %1$I p WHERE p.%2$I = %3$I.%4$I '
            'AND current_setting(''palai.project_id'', true) IS NOT NULL '
            'AND (p.project_id = current_setting(''palai.project_id'', true) OR p.project_id = ''''))',
            entry.parent, entry.parent_key, entry.child, entry.child_key);
        EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', entry.child);
        EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', entry.child);
        EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', entry.child);
        EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                       entry.child, expression, expression);
    END LOOP;
END
$$;

-- 4. The postcondition, asserted rather than described. `organizations` is the one expected survivor, so it
-- is named here — if it ever stops being the only one, this raises instead of the next migration finding a
-- policy comparing NULL to a GUC and returning zero rows with no error anywhere.
DO $$
DECLARE leftover TEXT;
BEGIN
    SELECT string_agg(tablename, ', ' ORDER BY tablename) INTO leftover
      FROM pg_policies
     WHERE schemaname = 'public'
       AND tablename <> 'organizations'
       AND (coalesce(qual, '') LIKE '%palai.org_id%' OR coalesce(with_check, '') LIKE '%palai.org_id%');
    IF leftover IS NOT NULL THEN
        RAISE EXCEPTION '000066 did not finish: these policies still read palai.org_id: %', leftover;
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (66) ON CONFLICT DO NOTHING;
