-- Reverse 000018: drop the tool-call ledger rider columns before 000001 drops the tool_calls table
-- itself. DROP COLUMN IF EXISTS keeps the rollback idempotent even after an earlier migration has
-- already removed the table (the 000014/000016 pattern).

ALTER TABLE tool_calls
    DROP COLUMN IF EXISTS commit_boundary,
    DROP COLUMN IF EXISTS reconciliation_state,
    DROP COLUMN IF EXISTS lease_owner,
    DROP COLUMN IF EXISTS external_idempotency_key,
    DROP COLUMN IF EXISTS request_hash,
    DROP COLUMN IF EXISTS replay_class;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 18;
    END IF;
END
$$;
