-- Reverse 000031: drop the secret-ref store. The tenant_isolation policy and the palai_app grant are
-- dropped with the table (neither can outlive it), so no explicit DROP POLICY / REVOKE is needed. A
-- rollback loses only the DB-backed secrets; the env-file bridge is untouched, so the fallback path stays.
DROP TABLE IF EXISTS secret_refs;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 31;
    END IF;
END
$$;
