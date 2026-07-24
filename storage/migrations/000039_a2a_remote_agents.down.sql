-- Reverse 000039: drop the A2A client registration table. Its tenant_isolation policy, index, and palai_app
-- grants are dropped with it (none can outlive its table), so no explicit DROP POLICY / REVOKE is needed. A
-- rollback loses the registered remote agents; no canonical run/session data lives here.
DROP TABLE IF EXISTS a2a_remote_agents;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 39;
    END IF;
END
$$;
