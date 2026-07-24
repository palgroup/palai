-- Reverse 000039: drop the CapabilityWorker contract tables. Each table's tenant_isolation policy, indexes,
-- UNIQUE constraints, and palai_app grants (and the capability_jobs REVOKE) are dropped with it (none can
-- outlive its table), so no explicit DROP POLICY / REVOKE is needed. The two tables are independent (no FK
-- between them — worker_id on the journal is a plain reference so the journal survives a worker's removal),
-- so drop order is free. A rollback loses the enrolled workers and the job journal; canonical runs are
-- untouched.
DROP TABLE IF EXISTS capability_jobs;
DROP TABLE IF EXISTS capability_workers;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 39;
    END IF;
END
$$;
