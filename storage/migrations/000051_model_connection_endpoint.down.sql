-- Reverse 000051. Guarded on the target still existing, because MigrationDown() is one concatenated chain
-- and re-running it must stay a clean no-op — 000050's, 000049's and 000048's reversals are written the
-- same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK: dropping base_url discards every custom endpoint an operator typed,
-- and there is nowhere else that address lives. A connection to a self-hosted vLLM then silently becomes a
-- connection to api.openai.com — the family's default — carrying a credential minted for a different
-- server. So a rollback across this line does not merely lose configuration; it re-points live routes.
-- Publish a new revision on a provider-one connection before crossing it, or delete the custom rows.
ALTER TABLE IF EXISTS model_connections DROP COLUMN IF EXISTS verification_outcome;
ALTER TABLE IF EXISTS model_connections DROP COLUMN IF EXISTS verified_at;
ALTER TABLE IF EXISTS model_connections DROP COLUMN IF EXISTS base_url;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 51;
    END IF;
END
$$;
