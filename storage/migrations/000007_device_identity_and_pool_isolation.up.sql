-- 000007 (device identity and pool isolation): make a machine's identity follow its DEVICE KEY rather
-- than its process, and give a pool a way to require the isolation mechanism it needs.
--
-- THE DEFECT THIS CLOSES, MEASURED RATHER THAN ARGUED. `packages/runner.Enroll` generated a fresh P-256
-- keypair on every call (enrollment.go, the `ecdsa.GenerateKey` at the top of the function), and
-- `cmd/runner` called it once per PROCESS START. So a Mac that rebooted enrolled as a machine the
-- registry had never seen: a new `rnr_` id, a new row, a second approval in a strict pool, and every
-- lifecycle decision an operator had made about the old row — cordon, revoke — reaching nothing.
-- `runners.public_key_sha256` already recorded the fingerprint and nothing keyed on it.
--
-- Measured on this tree 2026-08-05, before this file:
--
--     SELECT count(*) FROM runners;                                        -- rows
--     SELECT count(DISTINCT public_key_sha256) FROM runners;               -- distinct device keys
--
-- The two numbers were equal by construction, because no two calls to a fresh-keypair enrolment can
-- collide. That is exactly why the unique index below is safe to add and exactly why it was worth
-- nothing before: a constraint over a column whose values are unique BY ACCIDENT records no property.
--
--
-- WHAT THIS FILE DOES NOT DO. It does not backfill anything into `runners.agent_version` or
-- `isolation_modes`, and it does not assign an `isolation_mode` to any pool. An existing machine
-- reported neither fact and an existing pool required neither, so writing a value here would be
-- inventing an operator decision — the same reason 000006 left 104 secret names installation-wide.
-- Empty is "declared nothing" everywhere below, and a pool with no requirement admits every machine.


-- The columns. DEFAULT '' NOT NULL so the ALTER rewrites existing rows to the "declared nothing" marker
-- and so this file is re-runnable: the whole forward chain re-applies on every boot.
--
-- `agent_version` is inventory. The §48.2 support window is enforced at CONNECT (RunnerGateway's
-- Session.Version check), not here, and this column deliberately does not become a second enforcement
-- point — two places deciding whether a build is admissible is two places that can disagree.
--
-- `isolation_modes` is a comma-joined sorted list rather than a text[] for one reason worth stating: the
-- registry's read path scans every column of `runners` into one Go struct in four statements, and a text
-- column needs no per-statement array handling in any of them. The cost is that a membership question is
-- a string operation rather than an operator; the check that matters happens in Go before the row is
-- written (fleet.Store.Register), so no SQL here has to ask it.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS agent_version   text DEFAULT ''::text NOT NULL;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS isolation_modes text DEFAULT ''::text NOT NULL;

-- The pool's REQUIREMENT. Three legal values plus the empty string, and the empty string is what every
-- pool alive today carries: `user` and `accounts` are the two macOS postures (plan §3.5 — `user` is
-- same-customer accident isolation and NOT a cross-customer boundary, which is why DoD 19 makes
-- multi-tenant pools accounts-only), and `container` is the Linux sandbox posture.
--
-- THE CHECK ADMITS '' DELIBERATELY. A NOT NULL column with a three-value CHECK would have failed the
-- ALTER on every existing row, and a default of 'container' would have silently imposed a requirement on
-- pools nobody configured — including every Mac pool, which would then have refused every Mac.
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS isolation_mode text DEFAULT ''::text NOT NULL;

DO $$
BEGIN
    IF to_regclass('public.runner_pools') IS NOT NULL THEN
        ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_isolation_mode_check;
        ALTER TABLE runner_pools
            ADD CONSTRAINT runner_pools_isolation_mode_check
            CHECK (isolation_mode = ANY (ARRAY[''::text, 'user'::text, 'accounts'::text, 'container'::text]));
    END IF;
END
$$;


-- THE CONSTRAINT THIS WHOLE FILE EXISTS FOR: one machine per device key per pool.
--
-- It is PARTIAL on a non-empty fingerprint, and that is not tidiness. `public_key_sha256` has
-- `DEFAULT ''::text NOT NULL` since 000001, so an unpartitioned unique index would refuse the SECOND row
-- that ever enrolled without one — and a control plane running with no registry-recording issuer writes
-- exactly that. The partial index says the true thing: a device key identifies a machine, and no key
-- identifies nothing.
--
-- WHAT IT BUYS OVER THE GO CHECK. fleet.Store.Register resolves the fingerprint and recovers the row
-- inside its transaction, so the ordinary path never needs this index. What the index decides is the
-- RACE: two enrolments of one device key arriving at two control-plane replicas at once both miss the
-- SELECT and both INSERT, and without a unique index both succeed — two rows, one key, and the machine's
-- id then depends on which reply arrived last. With it, one loses on 23505 and retries into the
-- recovery path. A structural invariant beats a well-behaved caller, because the caller is what changes.
--
-- IT IS ALSO THE READ INDEX. ResolveRunnerByFingerprint filters on exactly (pool_id, public_key_sha256),
-- so this index serves it and no second index is owed.
--
--
-- A PRE-EXISTING DUPLICATE STOPS THE CHAIN, LOUDLY AND BY NAME, and that is a decision rather than an
-- oversight. Two rows sharing one device key in one pool means two machines hold the same private key —
-- either a cloned disk image or a copied identity — and that is a fact an operator has to resolve, not
-- one a migration may pick a winner for. The bare Postgres error would name only the index; this names
-- the rows. See the HONEST CEILING below.
DO $$
DECLARE
    collisions text;
BEGIN
    IF to_regclass('public.runners') IS NULL THEN
        RETURN;
    END IF;

    -- ORDER BY inside string_agg: an unordered aggregate is not deterministic, and this tree has had
    -- three outcomes decided by that omission, two of them security decisions.
    SELECT string_agg(format('pool %s key %s (%s rows)', pool_id, public_key_sha256, n), '; ' ORDER BY pool_id, public_key_sha256)
      INTO collisions
      FROM (SELECT pool_id, public_key_sha256, count(*) AS n
              FROM runners
             WHERE public_key_sha256 <> ''
             GROUP BY pool_id, public_key_sha256
            HAVING count(*) > 1) duplicates;

    IF collisions IS NOT NULL THEN
        RAISE EXCEPTION '000007: two or more machines share one device key in the same pool, which means one private key exists on more than one machine. No migration may choose which row keeps the identity. Resolve it (revoke the machines that should not hold that key) and re-run. Colliding groups: %', collisions;
    END IF;
END
$$;

CREATE UNIQUE INDEX IF NOT EXISTS runners_pool_device_key_key
    ON runners USING btree (pool_id, public_key_sha256)
 WHERE (public_key_sha256 <> ''::text);


-- HONEST CEILING, NAMED RATHER THAN LEFT FOR A READER TO TRIP OVER.
--
-- (1) The RAISE EXCEPTION above was NOT measured against a database that has a duplicate, because no
--     shipped code path could produce one: a fresh keypair per enrolment cannot collide. It is written
--     for the future in which device keys are durable — a cloned VM image carrying a `device.key` is the
--     realistic source — and its behaviour on that day is a claim from reading, not from running.
--
-- (2) This file adds no RLS policy and owes none. `runners` and `runner_pools` are already swept by
--     000002 and this migration creates no table; adding a column to an existing one inherits the
--     table's policy and grants unchanged. storage/tenant.go's rule binds a migration that CREATES a
--     tenant table.
--
-- (3) Nothing here enforces the isolation requirement. The column is the pool's stated need; the refusal
--     is in fleet.Store.Register, which compares it against the modes the MACHINE measured. A CHECK
--     cannot express that comparison because the two sides live in different rows.

INSERT INTO schema_migrations (version) VALUES (7) ON CONFLICT DO NOTHING;
