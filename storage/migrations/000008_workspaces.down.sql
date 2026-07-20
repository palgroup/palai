-- Reverse of 000008_workspaces.up.sql. Drops the four workspace tables in reverse dependency
-- order (snapshots and leases reference allocations and workspaces; allocations reference
-- workspaces) before 000001 drops the sessions/runs/projects they key to. The single-writer
-- partial index drops with its table. DROP ... IF EXISTS keeps the rollback idempotent even
-- after 000001 has already removed the referenced tables (the 000005 pattern).

DROP TABLE IF EXISTS workspace_snapshots;
DROP TABLE IF EXISTS workspace_leases;
DROP TABLE IF EXISTS workspace_allocations;
DROP TABLE IF EXISTS workspaces;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 8;
    END IF;
END
$$;
