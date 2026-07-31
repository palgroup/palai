-- Reverse 000049. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000048's, 000047's and 000046's reversals are
-- written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: it loses nothing. 000049 added no column and no table, so every
-- fact the series reads still lives in usage_ledger afterwards. What a rollback costs is the scan the
-- index was preventing — GET /v1/usage/series keeps answering, at the 5042-buffer / 56 ms shape the
-- 000049 header measured rather than the 54-buffer / 7.6 ms one.
DROP INDEX IF EXISTS usage_ledger_tenant_series_idx;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 49;
    END IF;
END
$$;
