-- A SHARED MAC SERVES ONE TENANT AT A TIME, AND UNTIL THIS REVISION NOTHING SAID SO.
--
-- `runners.capacity` bounds how many SESSIONS a machine runs at once, which is a resource question. It
-- was the only ceiling AcquireLease carried, and the two are not the same question: the machines this
-- plane enrols are `capacity = 0` — "undeclared, admits everything" — and both of them are free-fleet
-- (`project_id IS NULL`), so on 2026-08-06 any number of DIFFERENT customers could hold concurrent
-- occupancies on one Mac. Measured on the live plane: one machine had already served `prj_local` and
-- `prj_29f677d2…`; they happened to take turns, and nothing would have stopped them overlapping.
--
-- ‼️ THE PLATFORM'S OWN DOCUMENTED POSITION IS THAT THEY CANNOT SHARE ONE.
-- docs/operations/mac-sessions.md §5: "Different customers can share a Mac — **no**", because `sudo` and
-- any local-root escalation defeats a uid boundary, and three such escalations were patched in 2026
-- alone. That row was a claim about what the product PERMITS, and no code enforced it. A uid boundary is
-- what protects one session from another SESSION; taking turns is what protects one customer from
-- another CUSTOMER, and turns have to be made compulsory by something.
--
-- ‼️ WHY THIS IS A FUNCTION AND NOT A `NOT EXISTS` IN THE STATEMENT — the same reason
-- palai_machine_open_occupancies is one, and the reason is sharper here because the failure would be
-- SILENT. runner_leases is under row-level security, so a subquery written inline runs under the
-- ACQUIRING tenant's scope and can see only that tenant's own rows. `NOT EXISTS (… project_id <> $2)`
-- would therefore match nothing, evaluate true, and admit every caller — a guard that reads exactly like
-- a boundary in review and refuses nobody. This tree already records that shape twice (a rule whose
-- selector went empty; a read-side boundary with a write-side hole), and a predicate over other tenants'
-- rows is precisely where it recurs.
--
-- WHAT CROSSES THE BOUNDARY IS ONE BOOLEAN. Not a row, not a session id, not the other tenant's name —
-- the caller learns only that a machine it already named is busy with someone else, which it must learn
-- to be refused at all.
--
-- STABLE rather than IMMUTABLE, for palai_machine_open_occupancies' reason: it reads a table, and
-- IMMUTABLE would let the planner fold it to a constant across a statement that also writes leases.
CREATE OR REPLACE FUNCTION palai_machine_held_by_another_tenant(machine TEXT, tenant TEXT)
RETURNS boolean
LANGUAGE sql
STABLE
SECURITY DEFINER
SET search_path = pg_catalog, public
AS $$
    SELECT EXISTS (
        SELECT 1
          FROM runner_leases
         WHERE runner_id = machine
           AND released_at IS NULL
           AND project_id IS DISTINCT FROM tenant
    );
$$;

REVOKE ALL ON FUNCTION palai_machine_held_by_another_tenant(TEXT, TEXT) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION palai_machine_held_by_another_tenant(TEXT, TEXT) TO palai_app;

-- This file creates no table and owes no policy: it adds a function, and storage/tenant.go's rule binds a
-- migration that CREATES a tenant table. The function reads one that 000002 already sweeps, which is
-- exactly why it is SECURITY DEFINER.

INSERT INTO schema_migrations (version) VALUES (8) ON CONFLICT DO NOTHING;
