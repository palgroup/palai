-- 000034 is the REAL expand/migrate/CONTRACT example (E15 T1): the trailing "contract" tranche that
-- DROPs usage_events, the table 000001 created and 000032 (usage_ledger) superseded. It is the concrete
-- thing docs/operations/upgrade.md's discipline describes — a table is only dropped a full release after
-- its replacement shipped and drained, once no in-rollback-window binary still reads it.
--
-- WHY THIS IS SAFE TO DROP NOW (the rollback-window rule, spec §48.5 / plan §T1):
--   * ZERO readers/writers. A grep across the tree finds no non-test, non-migration Go/SQL/Python
--     reference to usage_events; the single writer LP-0 once shaped never landed, and 000032's own
--     comment records the table was deliberately "left in place and unwritten".
--   * SUPERSEDED IN A PRIOR RELEASE. usage_ledger (000032) shipped in E13 — a release BEFORE this SH-2
--     work — so the expand (the successor table) and the migrate (writers pointed at it) already shipped
--     and drained. The application-rollback target (the N binary, E14/E13) reads usage_ledger, and never
--     touched usage_events, so dropping it now cannot break a rollback inside the support window.
--
-- FLAP NOTE (inherent to the re-running-chain model, harmless): 000001 re-runs at the HEAD of every
-- boot's chain and re-creates usage_events (CREATE TABLE IF NOT EXISTS), and this migration drops it at
-- the TAIL. So the empty table is created and dropped once per boot; the END-of-chain state is always
-- "absent". Retiring 000001's CREATE would remove the flap, but historical migrations are immutable here,
-- so the flap stands until 000001 itself is ever rewritten. Nothing writes the table, so nothing is lost.
DROP TABLE IF EXISTS usage_events;

INSERT INTO schema_migrations (version) VALUES (34) ON CONFLICT DO NOTHING;
