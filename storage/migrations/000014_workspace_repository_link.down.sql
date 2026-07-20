-- Reverse 000014: drop the session→binding link columns before the earlier migration drops the
-- workspaces table itself.

ALTER TABLE workspaces
    DROP COLUMN IF EXISTS requested_ref,
    DROP COLUMN IF EXISTS repository_binding_id;

DELETE FROM schema_migrations WHERE version = 14;
