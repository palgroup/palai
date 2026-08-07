-- THIS ONE DOES REVERSE, unlike 000009, and the difference is worth naming rather than leaving to look
-- like inconsistency: reversing a dropped COLUMN is one line whose shape can be read against the original
-- in 000001 at a glance, while reversing five dropped TABLES was ninety lines of DDL nothing compares to
-- their first spelling. A reversal you can verify is worth writing; one you cannot is worse than an
-- absence somebody can read.
--
-- The default and the NOT NULL come back with it, so a plane rolled back to 9 has the column 9 had. What
-- does not come back is any VALUE, and nothing is lost by that: every row carried the empty string,
-- because no caller ever set it.
ALTER TABLE approvals ADD COLUMN IF NOT EXISTS allowed_approver text DEFAULT ''::text NOT NULL;

-- Guarded, because Store.Rollback may reach this file after the journal itself is gone.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 10;
    END IF;
END
$$;
