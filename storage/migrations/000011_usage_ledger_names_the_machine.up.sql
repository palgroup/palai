-- `usage_ledger` learns which MACHINE a hold sat on. The meter is called `machine.minutes`; until this
-- statement, a row could not name the machine.
--
-- WHAT THE ROW CARRIED, and why that was not enough. `usage_ledger` has three optional subject columns —
-- `session_id`, `run_id`, `model_request_id`. A `machine.minutes` row filled exactly one of them:
--
--   * `session_id` — set.
--   * `run_id`     — deliberately EMPTY, and rightly so: one occupancy hosts many runs, so naming any of
--                    them would attribute a whole machine hold to whichever run happened to be last.
--                    packages/coordinator/occupancy_billing.go says this in its own comment.
--   * `model_request_id` — meaningless for a hold.
--
-- So the machine was recoverable only by PARSING `dedupe_key`, which is built as
-- `"lease:" + leaseID + ":machine.minutes"` and then joining `runner_leases`. A dedupe key exists to make
-- a settlement idempotent; reading identity out of its string shape is the kind of defeated design this
-- tree keeps finding in path and membership comparisons. Attribution belongs in a FIELD.
--
-- THE DATA WAS ALREADY IN HAND AND WAS BEING DROPPED. `SettleOccupancy` reads the closed occupancy, uses
-- `closed.RunnerID` two statements later to announce the freed slot, and did not write it. This migration
-- and the query beside it stop discarding it.
--
-- MEASURED BEFORE ADDING (2026-08-07):
--
--   grep -c 'runner_id' storage/queries/usage.sql          -> 0   (no query named it)
--   grep -rn 'runner_id' tests/component/fleet/machine_minutes_test.go -> 0   (nothing asserted it)
--   grep -rn 'usageEntry{' --include='*.go' packages/ apps/ | grep -v _test -> one funnel, few literals
--
-- NULLABLE ON PURPOSE, and not merely for the backfill. Every other meter in this ledger — tokens, runs,
-- storage — is settled by work that has no machine, and a NOT NULL column would force those writers to
-- invent one. The column is filled by exactly one meter, which is why `SettleUsage` passes it through
-- `nullif`: an empty string from any other settlement stays NULL rather than becoming the machine "".
--
-- HISTORICAL ROWS STAY NULL. A backfill would have to reconstruct the machine by parsing the very
-- dedupe_key this column exists to stop trusting, and a reconstructed value is indistinguishable from a
-- measured one once it is in the table. Rows settled before this migration honestly do not know their
-- machine, and NULL is how a ledger says that.
ALTER TABLE usage_ledger ADD COLUMN IF NOT EXISTS runner_id text;

-- The lookup this column exists to serve is "what did machine X cost this project over a window", so the
-- index leads with the machine and carries the time. Partial, because only one meter ever fills it.
CREATE INDEX IF NOT EXISTS usage_ledger_runner_occurred_idx
    ON usage_ledger (runner_id, occurred_at)
    WHERE runner_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (11) ON CONFLICT DO NOTHING;
