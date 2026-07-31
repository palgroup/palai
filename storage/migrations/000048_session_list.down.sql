-- Reverse 000048. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000047's, 000046's and 000045's reversals are
-- written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: dropping sessions.name DISCARDS every label an operator typed.
-- There is nowhere else that text lives — it is not derivable, which is the whole reason the column
-- exists — so a rollback across this line is a data loss, not just a schema change. The four indexes
-- go with it and cost only the scans they were preventing.
DROP INDEX IF EXISTS usage_ledger_session_idx;
DROP INDEX IF EXISTS runs_session_created_idx;
DROP INDEX IF EXISTS responses_session_created_idx;
DROP INDEX IF EXISTS sessions_tenant_keyset_idx;
ALTER TABLE IF EXISTS sessions DROP COLUMN IF EXISTS name;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 48;
    END IF;
END
$$;
