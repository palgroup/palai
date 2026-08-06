DROP FUNCTION IF EXISTS palai_machine_open_occupancies(TEXT);
-- Reverse 000003: take the occupancy back off runner_leases and leave the skeleton 000001 declared.
-- Every statement is guarded, because Store.Rollback runs the whole down chain to return a shared
-- component database to empty and may reach this after the table itself is gone.

DROP INDEX IF EXISTS runner_leases_session_idx;

DO $$
BEGIN
    IF to_regclass('public.runner_leases') IS NOT NULL THEN
        ALTER TABLE runner_leases DROP CONSTRAINT IF EXISTS runner_leases_session_id_fkey;
        ALTER TABLE runner_leases DROP CONSTRAINT IF EXISTS runner_leases_release_reason_check;
        ALTER TABLE runner_leases DROP COLUMN IF EXISTS release_reason;
        ALTER TABLE runner_leases DROP COLUMN IF EXISTS released_at;
        ALTER TABLE runner_leases DROP COLUMN IF EXISTS last_activity_at;
        ALTER TABLE runner_leases DROP COLUMN IF EXISTS started_at;
        ALTER TABLE runner_leases DROP COLUMN IF EXISTS session_id;
    END IF;
END
$$;

-- Guarded for the reason 000002's own marker delete is: the down chain runs 000003 before 000001, but a
-- re-run reaches this after 000001 has already dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 3;
    END IF;
END
$$;
