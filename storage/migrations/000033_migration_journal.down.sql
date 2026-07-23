-- Reverse 000033: drop the migration journal. Its palai_app grant is dropped with the table (a grant
-- cannot outlive its relation), so no explicit REVOKE is needed. Rolling back loses the applied-at /
-- checksum history; schema_migrations still records which versions applied.
DROP TABLE IF EXISTS schema_revisions;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 33;
    END IF;
END
$$;
