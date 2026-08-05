-- Reverse 000005: give `runners.capacity` back the shape 000001 declared — zero illegal again, and the
-- default back to 1. Every statement is guarded, because Store.Rollback runs the whole down chain to
-- return a shared component database to empty and may reach this after the table itself is gone.
--
-- THE ROWS ARE RAISED BEFORE THE CONSTRAINT IS, and the order is the whole of it: `CHECK (capacity > 0)`
-- cannot be added back while the rows this migration set to 0 are still 0, so re-adding the constraint
-- first would fail the rollback on any database that had ever run the forward file. Rows carrying an
-- operator's real declaration (anything > 0) are left exactly as they are.
DO $$
BEGIN
    IF to_regclass('public.runners') IS NOT NULL THEN
        ALTER TABLE runners ALTER COLUMN capacity SET DEFAULT 1;
        UPDATE runners SET capacity = 1 WHERE capacity = 0;
        ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_capacity_check;
        ALTER TABLE runners ADD  CONSTRAINT runners_capacity_check CHECK ((capacity > 0));
    END IF;
END
$$;

-- Guarded for the reason 000004's own marker delete is: the down chain runs 000005 before 000001, but a
-- re-run reaches this after 000001 has already dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 5;
    END IF;
END
$$;
