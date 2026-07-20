-- Reverse of 000016_delivered_messages.up.sql. Drops the delivered_messages table (its index goes
-- with it) before 000004/000001 drop the commands/runs it references. DROP ... IF EXISTS keeps the
-- rollback idempotent even after an earlier migration has already removed the referenced tables
-- (the 000013 pattern).

DROP TABLE IF EXISTS delivered_messages;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 16;
    END IF;
END
$$;
