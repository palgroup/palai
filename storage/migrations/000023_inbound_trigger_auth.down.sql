-- Reverse 000023: drop the three inbound-auth columns off triggers. DROP COLUMN IF EXISTS keeps the
-- rollback idempotent even after an earlier migration has already dropped the table (the 000018/rider
-- pattern).

ALTER TABLE IF EXISTS triggers
    DROP COLUMN IF EXISTS created_by,
    DROP COLUMN IF EXISTS inbound_secret_ref,
    DROP COLUMN IF EXISTS inbound_secret_ref_next;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 23;
    END IF;
END
$$;
