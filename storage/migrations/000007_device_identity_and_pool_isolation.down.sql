-- Reverse 000007: give runners and runner_pools back the shape 000005 left them in.
--
-- Every statement is guarded, because Store.Rollback runs the whole down chain to return a shared
-- component database to empty and may reach this file after the tables themselves are gone.
--
-- THE INDEX COMES OFF FIRST. It is a dependent object of `public_key_sha256`, which this file does not
-- drop — that column is 000001's — so strictly the order is free here. It is still written first because
-- the forward file's order is columns-then-index and a reversal that reads as the mirror is a reversal a
-- reader can check.
--
-- WHAT THIS REVERSAL LOSES, STATED RATHER THAN GLOSSED: every machine's reported agent version, its
-- measured isolation modes, and every pool's isolation requirement. The identities themselves survive —
-- `public_key_sha256` is 000001's column and the rows keyed on it stay exactly as they are — so a
-- database rolled back and rolled forward again re-derives all three from the next enrolment of each
-- machine. That is why this reversal is serviceable where 000006's is not: nothing here is a fact only
-- the operator knew.
DROP INDEX IF EXISTS runners_pool_device_key_key;

DO $$
BEGIN
    IF to_regclass('public.runner_pools') IS NOT NULL THEN
        ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_isolation_mode_check;
        ALTER TABLE runner_pools DROP COLUMN IF EXISTS isolation_mode;
    END IF;
    IF to_regclass('public.runners') IS NOT NULL THEN
        ALTER TABLE runners DROP COLUMN IF EXISTS isolation_modes;
        ALTER TABLE runners DROP COLUMN IF EXISTS agent_version;
    END IF;
END
$$;

-- Guarded for the reason 000004's, 000005's and 000006's marker deletes are: the down chain runs 000007
-- before 000001, but a re-run reaches this after 000001 has already dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 7;
    END IF;
END
$$;
