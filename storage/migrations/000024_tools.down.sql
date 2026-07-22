-- Reverse 000024: drop the four E12 riders off both revision tables, then the registry tables in
-- reverse dependency order (tool_revisions references tools; tool_set_revisions is independent). DROP
-- COLUMN / TABLE IF EXISTS keeps the rollback idempotent even after an earlier migration dropped the
-- carrier table (the 000019/000023 pattern).
ALTER TABLE IF EXISTS agent_revisions
    DROP COLUMN IF EXISTS tool_sets,
    DROP COLUMN IF EXISTS mcp_connections,
    DROP COLUMN IF EXISTS skills,
    DROP COLUMN IF EXISTS hooks;
ALTER TABLE IF EXISTS run_template_revisions
    DROP COLUMN IF EXISTS tool_sets,
    DROP COLUMN IF EXISTS mcp_connections,
    DROP COLUMN IF EXISTS skills,
    DROP COLUMN IF EXISTS hooks;
DROP TABLE IF EXISTS tool_set_revisions;
DROP TABLE IF EXISTS tool_revisions;
DROP TABLE IF EXISTS tools;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 24;
    END IF;
END
$$;
