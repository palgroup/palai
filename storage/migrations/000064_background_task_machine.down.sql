-- Reverse of 000064. Dropping the column returns every row to "which machine is unknown", which the
-- reader treats as `lost` — the safe direction, and the state those rows had before the column existed.
DROP INDEX IF EXISTS background_tasks_running_machine_idx;
ALTER TABLE background_tasks DROP COLUMN IF EXISTS machine_id;
