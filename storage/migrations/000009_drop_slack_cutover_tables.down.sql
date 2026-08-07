-- THIS DOWN DOES NOT RECREATE THE FIVE TABLES, and that is a decision rather than an omission.
--
-- A down that reversed this one would hand-copy ~90 lines of DDL out of 000001 — five CREATE TABLEs, their
-- primary keys, their eight foreign keys, three indexes and five `CALL palai_apply_tenant_policy` lines —
-- and that copy would be a SECOND SPELLING of tables whose first spelling it can never be checked against.
-- Nothing in this tree compares the two, so the day 000001 changed, the reversal would quietly stop being
-- one. This tree already records what a second spelling costs; a reversal nobody can verify is worse than
-- an absence somebody can read.
--
-- AND WHAT IT WOULD RESTORE IS NOTHING ANYONE READS. At version 8 these tables were ALREADY dead — the
-- cutover removed their code before this migration existed, which is the whole reason it exists — so a
-- plane rolled back to 8 with them recreated would hold five empty tables no code path can reach, exactly
-- as it did the day before this ran. The data cannot come back either way: it is dropped by the statement
-- above and there were zero rows to drop.
--
-- WHAT ROLLING BACK STILL GIVES YOU is what a rollback is actually for here: Store.Rollback runs the whole
-- down chain to return a shared component database to empty, and 000001's own down drops these five by
-- name inside a single `DROP TABLE IF EXISTS` list — so the chain reaches empty from either side of this
-- migration, with or without the tables present. Verified rather than assumed: `IF EXISTS` covers that
-- whole list, so 000001's down does not error on a database this migration has already emptied.
--
-- To bring them back, re-run the chain from a fresh database. That is the honest instruction, and it is
-- the same one that applies to any migration that drops something.

-- Guarded, because Store.Rollback may reach this file after the journal itself is gone.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 9;
    END IF;
END
$$;
