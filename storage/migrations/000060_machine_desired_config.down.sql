-- Reverse of 000060_machine_desired_config.up.sql.
--
-- THE ROWS GO BEFORE THE CONSTRAINT, and that ordering is the whole of this file's difficulty. Restoring
-- the two-plane CHECK against a table holding `runner_machine` rows fails — Postgres validates an added
-- CHECK against existing data — so a down migration that only restated the constraint would refuse to run
-- on exactly the deployments that had used the feature. Deleting them is correct rather than merely
-- expedient: the plane those rows name is unreadable to the schema being restored, so they are documents
-- nothing can resolve.
--
-- IT IS A DELETE FROM AN APPEND-ONLY JOURNAL, which the runtime role cannot do (000052's REVOKE) and this
-- migration can, because migrations run as the owner. That is the intended asymmetry and it is worth
-- stating: append-only is a property the PROCESS has, not a property the schema's owner has, and a
-- migration that could not remove what it added would not be a down migration.
DELETE FROM deployment_desired WHERE plane = 'runner_machine';

ALTER TABLE deployment_desired
  DROP CONSTRAINT IF EXISTS deployment_desired_plane_check;
ALTER TABLE deployment_desired
  DROP CONSTRAINT IF EXISTS deployment_desired_scope_shape_check;

ALTER TABLE deployment_desired
  ADD CONSTRAINT deployment_desired_plane_check
  CHECK (plane IN ('control_plane', 'runner_pool'));

ALTER TABLE deployment_desired
  ADD CONSTRAINT deployment_desired_scope_shape_check
  CHECK (
       (plane = 'control_plane' AND scope_id =  '')
    OR (plane = 'runner_pool'   AND scope_id <> '')
  );

ALTER TABLE runners
  DROP CONSTRAINT IF EXISTS runners_config_applied_is_object;

ALTER TABLE runners
  DROP COLUMN IF EXISTS config_revision,
  DROP COLUMN IF EXISTS config_applied,
  DROP COLUMN IF EXISTS config_reported_at;
