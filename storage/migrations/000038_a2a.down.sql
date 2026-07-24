-- Reverse 000038: drop the A2A server-projection tables. Each table's tenant_isolation policy, indexes,
-- UNIQUE constraints, and palai_app grants are dropped with it (none can outlive its table), so no explicit
-- DROP POLICY / REVOKE is needed. a2a_task_refs references a2a_interfaces, so drop it first. A rollback
-- loses the external A2A task refs and published interfaces; the canonical runs/sessions are untouched.
DROP TABLE IF EXISTS a2a_task_refs;
DROP TABLE IF EXISTS a2a_interfaces;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 38;
    END IF;
END
$$;
