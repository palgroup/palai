-- This one reverses cleanly: one table, two indexes, one policy that goes with the table.
--
-- IT DESTROYS EVERY LINE A MACHINE HAS SHIPPED, and that is the cost to weigh rather than a caveat.
-- The agent keeps its own local log on each machine (~/Library/Logs/Palai/agent.log on a Mac), so the
-- information is not gone from the fleet — it is gone from the one place an operator could read it
-- without logging into a hundred machines.
DROP INDEX IF EXISTS runner_logs_session_idx;
DROP INDEX IF EXISTS runner_logs_runner_at_idx;
DROP TABLE IF EXISTS runner_logs;

-- Guarded, because Store.Rollback may reach this file after the journal itself is gone.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 12;
    END IF;
END
$$;
