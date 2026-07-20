-- Reverse 000012_tasks: drop the registry table, then withdraw the version marker. The guarded
-- delete survives an earlier migration having already dropped schema_migrations on a full rollback.
DROP TABLE IF EXISTS tasks;

DO $$ BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 12;
    END IF;
END $$;
