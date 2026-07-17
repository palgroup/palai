-- Reverse of 000001_core.up.sql. CASCADE drops the terminal-run trigger with runs;
-- dropping every table first clears the palai_app grants so the role can be removed,
-- which keeps up -> down -> up reapply clean.

DROP TABLE IF EXISTS
    schema_migrations,
    audit_events,
    usage_events,
    artifacts,
    tool_calls,
    model_requests,
    model_route_revisions,
    model_routes,
    model_connections,
    runner_leases,
    runners,
    runner_pools,
    inbox,
    outbox,
    job_attempts,
    durable_jobs,
    events,
    session_sequences,
    attempts,
    runs,
    messages,
    responses,
    sessions,
    idempotency_records,
    api_keys,
    principals,
    projects,
    organizations
CASCADE;

DROP FUNCTION IF EXISTS enforce_run_terminal_final();

DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'palai_app') THEN
        EXECUTE 'DROP OWNED BY palai_app';
        DROP ROLE palai_app;
    END IF;
END
$$;
