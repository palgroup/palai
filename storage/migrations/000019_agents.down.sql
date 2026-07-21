-- Reverse 000019: drop the run pin riders before the tables they reference, then the tables in
-- reverse dependency order (revisions reference profiles).
ALTER TABLE runs DROP COLUMN IF EXISTS run_template_revision_id;
ALTER TABLE runs DROP COLUMN IF EXISTS agent_revision_id;
DROP TABLE IF EXISTS run_template_revisions;
DROP TABLE IF EXISTS agent_revisions;
DROP TABLE IF EXISTS agent_profiles;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 19;
    END IF;
END
$$;
