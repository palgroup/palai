-- Reverse 000052: drop the desired-configuration journal. Its palai_app grant is dropped with the table
-- (a grant cannot outlive its relation), so no explicit REVOKE is needed.
--
-- Rolling back loses every recorded desired revision. Nothing running is affected by that: the desired
-- document is read at BRING-UP and turned into the process's environment, so a control plane already
-- running keeps the environment it was started with. What is lost is the record of what an operator asked
-- for and when — and the next bring-up falls back to the compose file's own defaults, which is the
-- behaviour of every deployment before this migration.
DROP TABLE IF EXISTS deployment_desired;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 53;
    END IF;
END
$$;
