-- Reverse of 000057_repository_binding_lifecycle.up.sql. The index goes first: dropping the column would
-- take it anyway, but naming both keeps the down migration readable as the inverse of the up.
DROP INDEX IF EXISTS repository_bindings_live_idx;

ALTER TABLE repository_bindings
  DROP COLUMN IF EXISTS archived_at;
