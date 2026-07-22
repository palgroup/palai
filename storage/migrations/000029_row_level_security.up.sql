-- 000029 turns tenant isolation from an application convention into a database guarantee (E13 Task 1,
-- TEN-001/TEN-002). Until now every tenant boundary lived in a hand-written WHERE clause: one omitted
-- predicate in one query leaked another organization's rows. This migration enables and FORCES row
-- level security on every tenant-scoped table and grants visibility only to the organization named by
-- the `palai.org_id` GUC, which the API's verified scope sets (never a body field). That GUC is
-- session-level, set once per pool acquisition by storage.OpenPool (set_config is_local=false), NOT
-- per transaction — the scope a borrowed connection publishes cannot outlive the acquisition that set it.
--
-- The runtime role is 000001's already-declared, never-used `palai_app`: NOLOGIN, not a table owner,
-- not superuser. storage.OpenPool switches every application connection onto it with SET ROLE, so the
-- policies below actually apply — RLS is inert for a superuser or a table owner.
--
-- THREE GUCs shape a policy decision, all read with the missing_ok form so an unset GUC is NULL and
-- therefore denies:
--   palai.org_id      the organization the caller is verified as. Unset => zero tenant rows.
--   palai.project_id  optional intra-tenant narrowing. Unset (or '') => the whole organization.
--   palai.system      'on' for the coordinator's genuinely cross-tenant infrastructure paths (the job
--                     claim loop, the retention sweep, the delivery pumps, API-key verification). It
--                     is a deliberate, greppable escape hatch, not a default: a context that declares
--                     nothing gets nothing.
--
-- HONEST CEILING: one database, one runtime role reached by SET ROLE from the owner connection. This
-- makes a missing WHERE clause in application SQL unexploitable. It does NOT defend against a
-- compromised control-plane process (which can RESET ROLE back to the owner), against a hostile DBA,
-- or provide encryption at rest — those are E13-H/E15. The forward step is purely operational: point
-- PALAI_DATABASE_URL at a palai_app LOGIN role and the same code enforces the same policies with no
-- RESET ROLE available.

-- The policy body, assembled once so all ~65 tables carry the IDENTICAL rule and a future table cannot
-- drift into a subtly weaker variant. tenant_key is the expression naming the row's organization.
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

-- Applies the isolation policy to one table, idempotently (DROP ... IF EXISTS then CREATE, so a re-run
-- of the chain re-asserts the CURRENT rule rather than leaving an older policy in place).
CREATE OR REPLACE PROCEDURE palai_apply_tenant_policy(target TEXT, tenant_key TEXT, has_project BOOLEAN)
LANGUAGE plpgsql
AS $$
DECLARE
    expression TEXT := palai_tenant_policy_expression(tenant_key, has_project);
BEGIN
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', target);
    -- FORCE so the boundary holds even if the table owner is ever a non-superuser service account;
    -- without it the owner silently bypasses its own policies.
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', target);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', target);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                   target, expression, expression);
END
$$;

DO $$
DECLARE
    entry RECORD;
BEGIN
    -- Every table that carries organization_id is secured by that column directly. Driving this off the
    -- catalogue rather than a hand-copied list means a LATER migration's tenant table is covered the
    -- moment the chain re-runs at boot, and tests/security/tenancy fails loudly if one is not.
    FOR entry IN
        SELECT c.relname AS table_name,
               EXISTS (SELECT 1 FROM information_schema.columns col
                        WHERE col.table_schema = 'public' AND col.table_name = c.relname
                          AND col.column_name = 'project_id') AS has_project
          FROM pg_class c
          JOIN pg_namespace n ON n.oid = c.relnamespace
         WHERE n.nspname = 'public'
           AND c.relkind = 'r'
           AND EXISTS (SELECT 1 FROM information_schema.columns col
                        WHERE col.table_schema = 'public' AND col.table_name = c.relname
                          AND col.column_name = 'organization_id')
    LOOP
        CALL palai_apply_tenant_policy(entry.table_name, 'organization_id', entry.has_project);
    END LOOP;
END
$$;

-- The tenant root itself: an organization row is visible to the organization it IS.
CALL palai_apply_tenant_policy('organizations', 'id', false);

-- Child tables that carry no tenant column of their own. Each is reached only through its parent, so
-- the policy resolves the parent's organization explicitly rather than leaning on nested RLS — the
-- rule is then readable in one place and does not depend on how Postgres evaluates policy subqueries.
ALTER TABLE delivery_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE delivery_attempts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON delivery_attempts;
CREATE POLICY tenant_isolation ON delivery_attempts FOR ALL TO PUBLIC
    USING (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM webhook_deliveries d
                       WHERE d.id = delivery_attempts.delivery_id
                         AND d.organization_id = current_setting('palai.org_id', true)))
    WITH CHECK (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM webhook_deliveries d
                       WHERE d.id = delivery_attempts.delivery_id
                         AND d.organization_id = current_setting('palai.org_id', true)));

ALTER TABLE job_attempts ENABLE ROW LEVEL SECURITY;
ALTER TABLE job_attempts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON job_attempts;
CREATE POLICY tenant_isolation ON job_attempts FOR ALL TO PUBLIC
    USING (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM durable_jobs j
                       WHERE j.id = job_attempts.job_id
                         AND j.organization_id = current_setting('palai.org_id', true)))
    WITH CHECK (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM durable_jobs j
                       WHERE j.id = job_attempts.job_id
                         AND j.organization_id = current_setting('palai.org_id', true)));

ALTER TABLE model_route_revisions ENABLE ROW LEVEL SECURITY;
ALTER TABLE model_route_revisions FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON model_route_revisions;
CREATE POLICY tenant_isolation ON model_route_revisions FOR ALL TO PUBLIC
    USING (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM model_routes r
                       WHERE r.id = model_route_revisions.route_id
                         AND r.organization_id = current_setting('palai.org_id', true)))
    WITH CHECK (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM model_routes r
                       WHERE r.id = model_route_revisions.route_id
                         AND r.organization_id = current_setting('palai.org_id', true)));

ALTER TABLE schedule_occurrences ENABLE ROW LEVEL SECURITY;
ALTER TABLE schedule_occurrences FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS tenant_isolation ON schedule_occurrences;
CREATE POLICY tenant_isolation ON schedule_occurrences FOR ALL TO PUBLIC
    USING (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM schedules s
                       WHERE s.id = schedule_occurrences.schedule_id
                         AND s.organization_id = current_setting('palai.org_id', true)))
    WITH CHECK (coalesce(current_setting('palai.system', true), '') = 'on'
           OR EXISTS (SELECT 1 FROM schedules s
                       WHERE s.id = schedule_occurrences.schedule_id
                         AND s.organization_id = current_setting('palai.org_id', true)));

-- The runtime role must hold DML on every tenant table, or RLS is not the thing gating it: a missing
-- GRANT fails closed as a blunt "permission denied for table" instead of the row-scoped policy. Several
-- tables created after 000001 (commands, config_revisions, delivered_messages, skills, …) were never
-- re-granted — they worked only because the app used to connect AS the owner. This is where the role
-- becomes load-bearing, so assert the complete set here, then re-apply the append-only REVOKEs so audit
-- and checkpoint/boundary immutability (spec §50.3, §26.1) still hold.
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO palai_app;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO palai_app;
REVOKE UPDATE, DELETE ON audit_events FROM palai_app;
REVOKE UPDATE ON transcript_boundaries, checkpoints FROM palai_app;

-- Deliberately NOT under RLS, and named here so the omission is a decision rather than an oversight:
--   schema_migrations  the chain's own ledger, written by the owner before any tenant exists.
--   host_quarantine    coordinator infrastructure keyed by host, holding no tenant data.
--   session_sequences  a session id and a monotonic integer; no tenant payload to leak.
-- tests/security/tenancy asserts this list and nothing else escapes.

INSERT INTO schema_migrations (version) VALUES (29) ON CONFLICT DO NOTHING;
