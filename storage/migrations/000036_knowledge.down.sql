-- Reverse 000036: drop the knowledge spine. The tenant_isolation policies and palai_app grants are dropped
-- with the tables (neither outlives its table), so no explicit DROP POLICY / REVOKE is needed. Drop in FK
-- dependency order — chunk/index/document revisions and jobs/sources reference knowledge_bases.
DROP TABLE IF EXISTS chunk_revisions;
DROP TABLE IF EXISTS index_revisions;
DROP TABLE IF EXISTS document_revisions;
DROP TABLE IF EXISTS ingestion_jobs;
DROP TABLE IF EXISTS knowledge_sources;
DROP TABLE IF EXISTS knowledge_bases;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 36;
    END IF;
END
$$;
