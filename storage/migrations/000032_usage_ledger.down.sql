-- Reverse 000032: drop the settlement ledger and the two durable admission limits. Each table's
-- tenant_isolation policy, indexes, and palai_app grants are dropped with it (none can outlive its
-- table), so no explicit DROP POLICY / REVOKE is needed. A rollback loses the metering history and
-- un-caps admission; the runs themselves are untouched.
DROP TABLE IF EXISTS quotas;
DROP TABLE IF EXISTS budgets;
DROP TABLE IF EXISTS usage_ledger;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 32;
    END IF;
END
$$;
