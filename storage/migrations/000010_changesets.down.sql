-- Reverse of 000010_changesets.up.sql. Drops the two tables in reverse dependency order
-- (changeset_findings references changesets) and removes the richer artifact columns before 000001
-- drops the artifacts/runs/projects they key to. DROP ... IF EXISTS keeps the rollback idempotent
-- even after 000001 has already removed the referenced tables (the 000009/000011 pattern).

DROP TABLE IF EXISTS changeset_findings;
DROP TABLE IF EXISTS changesets;

ALTER TABLE IF EXISTS artifacts DROP COLUMN IF EXISTS media_type;
ALTER TABLE IF EXISTS artifacts DROP COLUMN IF EXISTS logical_type;
ALTER TABLE IF EXISTS artifacts DROP COLUMN IF EXISTS malware_scan_status;
ALTER TABLE IF EXISTS artifacts DROP COLUMN IF EXISTS provenance;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 10;
    END IF;
END
$$;
