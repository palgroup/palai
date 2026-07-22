-- Reverse 000030: drop the api_keys provisioning columns and restore the pre-000030 grant on the migration
-- ledger. The api_keys tenant_isolation policy is NOT reversed here — migration 000029 owns it (its
-- catalogue loop re-asserts it every boot), and dropping the two columns does not touch it.
ALTER TABLE api_keys DROP COLUMN IF EXISTS scopes;
ALTER TABLE api_keys DROP COLUMN IF EXISTS expires_at;

-- Restore the write grant M1 revoked (000029's blanket GRANT ON ALL TABLES had handed it to palai_app).
GRANT INSERT, UPDATE, DELETE ON schema_migrations TO palai_app;

-- M2's role-membership grant is additive operational hardening and may predate this migration on a managed
-- deployment; revoking it could break an owner's ability to SET ROLE that never depended on 000030. It is
-- left in place deliberately (reversing it is unsafe, granting it is idempotent).

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 30;
    END IF;
END
$$;
