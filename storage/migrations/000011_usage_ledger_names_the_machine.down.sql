-- Reversing this one is two statements whose shape reads against the up file line for line, so it is
-- written rather than refused.
--
-- IT DESTROYS ATTRIBUTION, and that is the honest cost rather than a caveat: the machine ids settled while
-- this migration was applied exist ONLY in this column. Nothing else in the row carries them — `run_id` is
-- deliberately empty for this meter — so a plane rolled back to 10 has holds whose machine is unrecoverable
-- except by parsing `dedupe_key`, which is exactly the reading the column was added to stop. Dropping the
-- index first is ordinary; dropping the column is the part to think about before running this.
DROP INDEX IF EXISTS usage_ledger_runner_occurred_idx;

ALTER TABLE usage_ledger DROP COLUMN IF EXISTS runner_id;

-- Guarded, because Store.Rollback may reach this file after the journal itself is gone.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 11;
    END IF;
END
$$;
