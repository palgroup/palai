-- Reverse 000004: take the metadata column back off responses and leave the shape 000001 declared.
-- Every statement is guarded, because Store.Rollback runs the whole down chain to return a shared
-- component database to empty and may reach this after the table itself is gone.

DO $$
BEGIN
    IF to_regclass('public.responses') IS NOT NULL THEN
        ALTER TABLE responses DROP COLUMN IF EXISTS metadata;
    END IF;
END
$$;

-- Guarded for the reason 000003's own marker delete is: the down chain runs 000004 before 000001, but a
-- re-run reaches this after 000001 has already dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 4;
    END IF;
END
$$;
