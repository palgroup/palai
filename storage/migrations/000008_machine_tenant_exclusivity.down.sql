-- Dropping this function makes AcquireLease unrunnable rather than merely unguarded, which is the
-- intended shape of a down: the statement names it, so a plane rolled back to 7 refuses every lease
-- instead of quietly admitting two tenants onto one Mac again.
DROP FUNCTION IF EXISTS palai_machine_held_by_another_tenant(TEXT, TEXT);

-- Guarded, because Store.Rollback runs the whole down chain to return a shared component database to
-- empty and may reach this file after the journal itself is gone.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 8;
    END IF;
END
$$;
