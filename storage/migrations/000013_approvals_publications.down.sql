-- Reverse of 000013_approvals_publications.up.sql. Drops approvals before publications (approvals
-- references publications), and both before 000001 drops the runs/projects they key to. DROP ... IF
-- EXISTS keeps the rollback idempotent even after 000001 has already removed the referenced tables
-- (the 000010/000012 pattern).

DROP TABLE IF EXISTS approvals;
DROP TABLE IF EXISTS publications;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 13;
    END IF;
END
$$;
