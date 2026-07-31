-- Reverse 000050. Guarded on the target still existing, because MigrationDown() is one concatenated
-- chain and re-running it must stay a clean no-op — 000049's and 000048's reversals are written the
-- same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: it loses nothing that cannot be recomputed. Every value in this
-- column was derived from dedupe_key and is still derivable from it — the forward migration's own
-- backfill is the recipe. What a rollback costs is that reading a turn's cost goes back to parsing a
-- string whose format nothing constrains.
ALTER TABLE IF EXISTS usage_ledger DROP COLUMN IF EXISTS model_request_id;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 50;
    END IF;
END
$$;
