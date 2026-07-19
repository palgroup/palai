-- Reverse of 000005_config_revisions.up.sql. Drops the config_revisions table before 000001
-- drops the tables it references, and drops the projects.config_policy column. DROP ... IF
-- EXISTS keeps the rollback idempotent even after 000001 has already removed the tables.

DROP TABLE IF EXISTS config_revisions;

ALTER TABLE IF EXISTS projects DROP COLUMN IF EXISTS config_policy;

-- Guarded so the rollback stays idempotent even after 000001 has dropped the table.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 5;
    END IF;
END
$$;
