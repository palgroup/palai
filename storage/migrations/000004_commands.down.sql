-- Reverse of 000004_commands.up.sql. Drops the commands table (and its index with it)
-- before 000001 drops the tables it references. DROP ... IF EXISTS keeps the rollback
-- idempotent even after 000001 has already removed the referenced tables.

DROP TABLE IF EXISTS commands;

-- Guarded so the rollback stays idempotent even after 000001 has dropped the table.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 4;
    END IF;
END
$$;
