-- 000005 (capacity declaration): `runners.capacity` gains a way to say NOTHING, because until now every
-- machine said 1 and none of them meant it.
--
-- WHAT WAS ACTUALLY STORED, measured 2026-08-05 immediately before this file:
--
--     docker exec palai-a1e00dad-postgres-1 psql -U palai -d palai -tAc \
--       "select count(*), string_agg(distinct capacity::text, ',') from runners"
--     -> 44 | 1
--
-- Forty-four machines, one distinct value. That is not agreement, it is an artefact: the one production
-- `fleet.Registration` (runner_gateway.go's enrolment handler) sets every field EXCEPT Capacity,
-- `packages/runner` never sends the word, and storage/queries/runners.sql has no UPDATE that could change
-- one afterwards. So `reg.Capacity` was 0 on every enrolment there has ever been, and
-- internal/fleet/store.go clamped it to 1 on the way in.
--
-- WHY THE CLAMP COULD NOT SIMPLY BE DELETED, which is the reason this file exists at all:
--
--     000001_core.up.sql:908   CONSTRAINT runners_capacity_check CHECK ((capacity > 0))
--
-- The column REFUSES zero. `capacity integer DEFAULT 1 NOT NULL` leaves no storable value that means
-- "this machine declared nothing" — not 0, and not NULL. Removing the clamp without this migration makes
-- every enrolment fail on a constraint violation. The fiction was produced by the schema, and the clamp
-- was the buffer standing in front of it.
--
-- SO ZERO CHANGES MEANING RATHER THAN A COLUMN BEING ADDED. After this file:
--
--     capacity = 0   the machine declared nothing. NO ceiling is enforced (leases.sql's AcquireLease
--                    guards its ceiling with `r.capacity > 0`), which is exactly the behaviour every
--                    deployment has today.
--     capacity > 0   an operator said a number, and the placement ceiling holds the machine to it.
--
-- The ceiling therefore becomes OPT-IN, and today nothing opts in. That is the point: enforcing a 1 that
-- no one chose is enforcing a fiction, and this tree's own rule is that OVERSTATING a ceiling is as wrong
-- as understating it — it leaves the operator a demonstration that does not work when they run it.
--
-- This file CREATES NO TABLE, so it owes no policy call and no grant of its own — the rule in
-- storage/tenant.go binds a migration that creates a table carrying project_id. 000002's catalogue sweep
-- already secured `runners`, and 000001's table-level GRANT covers it.

-- The CHECK, replaced rather than dropped: zero becomes legal and negative stays illegal. A column whose
-- constraint was simply removed would accept -1, and a negative ceiling reads as "refuse everything" in
-- exactly the comparison this change exists to make.
DO $$
BEGIN
    IF to_regclass('public.runners') IS NOT NULL THEN
        ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_capacity_check;
        ALTER TABLE runners ADD  CONSTRAINT runners_capacity_check CHECK (capacity >= 0);
    END IF;
END
$$;

-- A machine that declares nothing now STORES nothing, rather than storing somebody's guess.
ALTER TABLE runners ALTER COLUMN capacity SET DEFAULT 0;

-- THE BACKFILL, AND IT IS GUARDED SO IT RUNS EXACTLY ONCE — the whole forward chain is re-applied on
-- every boot, and an unguarded `UPDATE runners SET capacity = 0` would erase every capacity an operator
-- ever declared, on every restart, forever. The marker is what makes a one-time DATA step one-time; every
-- other statement in this chain is idempotent by shape, and this one cannot be.
--
-- It is correct to run because of the measurement at the top: no declaration path has ever existed, so
-- every stored 1 is the clamp's artefact and not an operator's choice. Leaving them would be worse than
-- doing nothing — under the new meaning a stored 1 reads as "the operator declared one", so all
-- forty-four machines would become genuinely capped at a single session each, which is the fiction this
-- migration removes, now enforced.
--
-- `WHERE capacity = 1` IS THE MEASUREMENT WRITTEN AS A PREDICATE, and it is why this is not a bare UPDATE.
-- What is being erased is THE CLAMP'S ARTEFACT, not everybody's declaration, and the clamp only ever wrote
-- 1. On this tree the two are the same set of rows — `distinct_capacities=1` — so the predicate changes no
-- behaviour today; on an installation where that premise does not hold, a 5 somebody meant SURVIVES, which
-- is the right answer. A migration should carry its own premise in its statement, so that a database the
-- premise is false about is not quietly included.
DO $$
BEGIN
    IF to_regclass('public.runners') IS NOT NULL
       AND to_regclass('public.schema_migrations') IS NOT NULL
       AND NOT EXISTS (SELECT 1 FROM schema_migrations WHERE version = 5) THEN
        UPDATE runners SET capacity = 0 WHERE capacity = 1;
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (5) ON CONFLICT DO NOTHING;
