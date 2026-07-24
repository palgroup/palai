-- Reverse 000035: drop the Slack store. slack_thread_sessions references slack_connections, so it drops
-- first. Each table's tenant_isolation policy and palai_app grant are dropped with it (neither can outlive
-- the table), so no explicit DROP POLICY / REVOKE is needed. A rollback loses only the Slack bindings and
-- thread correlations; the secret_refs the handles pointed at are untouched.
DROP TABLE IF EXISTS slack_thread_sessions;
DROP TABLE IF EXISTS slack_connections;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 35;
    END IF;
END
$$;
