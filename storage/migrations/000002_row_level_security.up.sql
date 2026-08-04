-- 000002 turns tenant isolation into a database guarantee: ENABLE + FORCE ROW LEVEL SECURITY and one
-- policy per table, enforced against the non-owner palai_app role 000001 created and storage.OpenPool
-- switches every application connection onto.
--
-- IT IS A SEPARATE MIGRATION BECAUSE A POLICY NEEDS ITS TABLE. That was the old chain's reason for putting
-- this sweep at 000029 rather than at 000001, and squashing does not change it. Keeping the seam also keeps
-- an interrupted boot observable: stopping between these two files leaves tables present and unsecured,
-- which is a state the resume drill can be ABOUT. A one-file chain has no partway.
--
-- FOUR SHAPES, MEASURED FROM THE POLICIES THE OLD CHAIN LEFT BEHIND rather than reasoned out:
--   81 tables carry project_id and are keyed on it;
--    1 table is `projects` itself, keyed on its own id;
--    4 tables resolve their parent's project (the join column differs each time, so they are written out);
--    4 tables have no narrower key than the installation.
-- Five tables take no policy at all: deployment_desired and host_quarantine are installation-global by
-- design, schema_migrations and schema_revisions are the migration ledger, and session_sequences is keyed
-- only by its session. tests/security/tenancy names all five and fails a sixth that appears without one.
--
-- THE TENANT EXPRESSION LOST A PARAMETER. It used to take (tenant_key, has_project) and the body never read
-- has_project -- every call site passed false by the end of the old chain, and a parameter nothing reads is
-- a boundary a reader will believe in. It also lost a guard that skipped a table whose tenant_key column was
-- absent: that guard existed so the old chain's forty-odd calls keyed on the REMOVED boundary could no-op
-- once its column was dropped. With no such call left, the guard's only remaining effect would be to turn a
-- misspelled column into a SILENTLY UNSECURED TABLE.


-- The project-keyed expression. The empty-string arm is not a wildcard: a row whose project_id is '' is
-- installation-global data written before any tenant existed, and it stays readable to every scope. A
-- connection that declared NO project fails the IS NOT NULL arm and sees nothing -- storage.PrepareConn
-- refuses such an acquisition outright, and this is the second half of that fence.
CREATE OR REPLACE FUNCTION palai_tenant_policy_expression(tenant_key TEXT)
RETURNS TEXT
LANGUAGE sql IMMUTABLE
AS $_$
    SELECT format(
        'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
        'OR (current_setting(''palai.project_id'', true) IS NOT NULL '
        'AND (%1$s = current_setting(''palai.project_id'', true) OR %1$s = ''''))',
        tenant_key);
$_$;

-- The installation-wide expression. It is deliberately NOT `true`: a connection that never declared a scope
-- must still see nothing.
CREATE OR REPLACE FUNCTION palai_installation_policy_expression()
RETURNS TEXT
LANGUAGE sql IMMUTABLE
AS $$
    SELECT 'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
           'OR current_setting(''palai.project_id'', true) IS NOT NULL';
$$;

CREATE OR REPLACE PROCEDURE palai_apply_tenant_policy(target TEXT, tenant_key TEXT)
LANGUAGE plpgsql
AS $$
DECLARE expression TEXT := palai_tenant_policy_expression(tenant_key);
BEGIN
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', target);
    -- FORCE so the boundary holds even when the table owner is a non-superuser service account; without
    -- it the owner silently bypasses its own policies.
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', target);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', target);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                   target, expression, expression);
END
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

-- A child resolves its parent's project. Written out per table rather than swept: the parent and the
-- joining column differ every time, so a loop would need a hand-written table of them anyway and would
-- hide it inside a query.
CREATE OR REPLACE PROCEDURE palai_apply_child_policy(target TEXT, parent TEXT, join_column TEXT)
LANGUAGE plpgsql
AS $$
DECLARE expression TEXT;
BEGIN
    expression := format(
        'coalesce(current_setting(''palai.system'', true), '''') = ''on'' '
        'OR EXISTS (SELECT 1 FROM %I p WHERE p.id = %I.%I '
        'AND current_setting(''palai.project_id'', true) IS NOT NULL '
        'AND (p.project_id = current_setting(''palai.project_id'', true) OR p.project_id = ''''))',
        parent, target, join_column);
    EXECUTE format('ALTER TABLE %I ENABLE ROW LEVEL SECURITY', target);
    EXECUTE format('ALTER TABLE %I FORCE ROW LEVEL SECURITY', target);
    EXECUTE format('DROP POLICY IF EXISTS tenant_isolation ON %I', target);
    EXECUTE format('CREATE POLICY tenant_isolation ON %I FOR ALL TO PUBLIC USING (%s) WITH CHECK (%s)',
                   target, expression, expression);
END
$$;

-- The project-keyed tables.
CALL palai_apply_tenant_policy('a2a_interfaces', 'project_id');
CALL palai_apply_tenant_policy('a2a_remote_agents', 'project_id');
CALL palai_apply_tenant_policy('a2a_task_refs', 'project_id');
CALL palai_apply_tenant_policy('agent_profiles', 'project_id');
CALL palai_apply_tenant_policy('agent_revisions', 'project_id');
CALL palai_apply_tenant_policy('api_keys', 'project_id');
CALL palai_apply_tenant_policy('approvals', 'project_id');
CALL palai_apply_tenant_policy('artifacts', 'project_id');
CALL palai_apply_tenant_policy('attempts', 'project_id');
CALL palai_apply_tenant_policy('audit_events', 'project_id');
CALL palai_apply_tenant_policy('background_tasks', 'project_id');
CALL palai_apply_tenant_policy('budgets', 'project_id');
CALL palai_apply_tenant_policy('capability_jobs', 'project_id');
CALL palai_apply_tenant_policy('capability_workers', 'project_id');
CALL palai_apply_tenant_policy('changeset_findings', 'project_id');
CALL palai_apply_tenant_policy('changesets', 'project_id');
CALL palai_apply_tenant_policy('checkpoints', 'project_id');
CALL palai_apply_tenant_policy('chunk_revisions', 'project_id');
CALL palai_apply_tenant_policy('commands', 'project_id');
CALL palai_apply_tenant_policy('config_revisions', 'project_id');
CALL palai_apply_tenant_policy('delivered_messages', 'project_id');
CALL palai_apply_tenant_policy('document_revisions', 'project_id');
CALL palai_apply_tenant_policy('durable_jobs', 'project_id');
CALL palai_apply_tenant_policy('events', 'project_id');
CALL palai_apply_tenant_policy('hooks', 'project_id');
CALL palai_apply_tenant_policy('idempotency_records', 'project_id');
CALL palai_apply_tenant_policy('inbox', 'project_id');
CALL palai_apply_tenant_policy('index_revisions', 'project_id');
CALL palai_apply_tenant_policy('ingestion_jobs', 'project_id');
CALL palai_apply_tenant_policy('integration_bots', 'project_id');
CALL palai_apply_tenant_policy('knowledge_bases', 'project_id');
CALL palai_apply_tenant_policy('knowledge_sources', 'project_id');
CALL palai_apply_tenant_policy('mcp_connections', 'project_id');
CALL palai_apply_tenant_policy('merge_records', 'project_id');
CALL palai_apply_tenant_policy('messages', 'project_id');
CALL palai_apply_tenant_policy('model_connections', 'project_id');
CALL palai_apply_tenant_policy('model_requests', 'project_id');
CALL palai_apply_tenant_policy('model_routes', 'project_id');
CALL palai_apply_tenant_policy('outbox', 'project_id');
CALL palai_apply_tenant_policy('preparation_receipts', 'project_id');
CALL palai_apply_tenant_policy('principals', 'project_id');
CALL palai_apply_tenant_policy('publications', 'project_id');
CALL palai_apply_tenant_policy('queue_connections', 'project_id');
CALL palai_apply_tenant_policy('queue_deliveries', 'project_id');
CALL palai_apply_tenant_policy('queue_effect_receipts', 'project_id');
CALL palai_apply_tenant_policy('queue_messages', 'project_id');
CALL palai_apply_tenant_policy('quotas', 'project_id');
CALL palai_apply_tenant_policy('remote_tool_operations', 'project_id');
CALL palai_apply_tenant_policy('repository_bindings', 'project_id');
CALL palai_apply_tenant_policy('responses', 'project_id');
CALL palai_apply_tenant_policy('run_template_revisions', 'project_id');
CALL palai_apply_tenant_policy('runner_enrollments', 'project_id');
CALL palai_apply_tenant_policy('runner_leases', 'project_id');
CALL palai_apply_tenant_policy('runner_pool_keys', 'project_id');
CALL palai_apply_tenant_policy('runner_pools', 'project_id');
CALL palai_apply_tenant_policy('runners', 'project_id');
CALL palai_apply_tenant_policy('runs', 'project_id');
CALL palai_apply_tenant_policy('schedules', 'project_id');
CALL palai_apply_tenant_policy('sessions', 'project_id');
CALL palai_apply_tenant_policy('skill_revisions', 'project_id');
CALL palai_apply_tenant_policy('skills', 'project_id');
CALL palai_apply_tenant_policy('slack_approval_deliveries', 'project_id');
CALL palai_apply_tenant_policy('slack_connections', 'project_id');
CALL palai_apply_tenant_policy('slack_message_turns', 'project_id');
CALL palai_apply_tenant_policy('slack_reply_deliveries', 'project_id');
CALL palai_apply_tenant_policy('slack_thread_sessions', 'project_id');
CALL palai_apply_tenant_policy('tasks', 'project_id');
CALL palai_apply_tenant_policy('tool_calls', 'project_id');
CALL palai_apply_tenant_policy('tool_revisions', 'project_id');
CALL palai_apply_tenant_policy('tool_set_revisions', 'project_id');
CALL palai_apply_tenant_policy('tools', 'project_id');
CALL palai_apply_tenant_policy('transcript_boundaries', 'project_id');
CALL palai_apply_tenant_policy('trigger_deliveries', 'project_id');
CALL palai_apply_tenant_policy('trigger_revisions', 'project_id');
CALL palai_apply_tenant_policy('triggers', 'project_id');
CALL palai_apply_tenant_policy('webhook_deliveries', 'project_id');
CALL palai_apply_tenant_policy('webhook_endpoints', 'project_id');
CALL palai_apply_tenant_policy('workspace_allocations', 'project_id');
CALL palai_apply_tenant_policy('workspace_leases', 'project_id');
CALL palai_apply_tenant_policy('workspace_snapshots', 'project_id');
CALL palai_apply_tenant_policy('workspaces', 'project_id');

-- projects is keyed on its own id: a project row IS the tenant.
CALL palai_apply_tenant_policy('projects', 'id');

-- The children, each reading its parent's project.
CALL palai_apply_child_policy('delivery_attempts', 'webhook_deliveries', 'delivery_id');
CALL palai_apply_child_policy('job_attempts', 'durable_jobs', 'job_id');
CALL palai_apply_child_policy('model_route_revisions', 'model_routes', 'route_id');
CALL palai_apply_child_policy('schedule_occurrences', 'schedules', 'schedule_id');

-- The installation-wide tables: no key narrower than this installation exists for them.
CALL palai_apply_installation_policy('environment_values');
CALL palai_apply_installation_policy('environments');
CALL palai_apply_installation_policy('secret_refs');
CALL palai_apply_installation_policy('usage_ledger');

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT DO NOTHING;
