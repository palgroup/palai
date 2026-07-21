-- Reverse of 000015_recovery_objects.up.sql. Drop the checkpoint half first (it FKs both the
-- workspace snapshot and the transcript boundary), then the workspace_snapshots.boundary_id column
-- (it FKs the transcript boundary), then the boundary table — so no FK is orphaned mid-drop. This
-- runs BEFORE 000008 drops workspace_snapshots (the down chain reverses in version order), so the
-- ALTER's target still exists; DROP ... IF EXISTS keeps the rollback idempotent regardless.

DROP TABLE IF EXISTS checkpoints;

ALTER TABLE workspace_snapshots
    DROP COLUMN IF EXISTS boundary_id;

DROP TABLE IF EXISTS transcript_boundaries;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 15;
    END IF;
END
$$;
