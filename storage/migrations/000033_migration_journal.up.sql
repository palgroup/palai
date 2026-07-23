-- 000033 introduces schema_revisions: the append-only journal the boot migration runner writes ONE row
-- to per applied migration (E15 T1, OPS-006). schema_migrations already records WHICH versions applied;
-- this journal adds the evidence an upgrade or DR audit reads — WHEN each migration applied, by WHICH
-- binary version stamp, and the sha256 CHECKSUM of the migration file that applied it, so a re-applied
-- file whose bytes drifted from the one that first applied it is detectable, and so the chain's head can
-- be read back to prove an interrupted upgrade resumed to the expected version.
--
-- SCOPE: the runner records a row only for migrations from 000033 onward — the journal's own birth.
-- Earlier migrations predate it and stay recorded in schema_migrations alone (back-filling them would
-- have to invent an applied_at/applied_by they never had). The head is therefore always the true latest
-- version; the history is complete from the journal's introduction forward.
--
-- The rows are written by the runner under the OWNING role (it RESET ROLEs before every migration), never
-- by palai_app. This is not a tenant table — it is installation-global, so it takes NO row-level-security
-- policy (the 000029 catalogue loop only touches tables carrying organization_id).
CREATE TABLE IF NOT EXISTS schema_revisions (
    version    INTEGER PRIMARY KEY,          -- the migration number (matches schema_migrations.version)
    checksum   TEXT NOT NULL,                -- sha256 of the migration's up.sql, lowercase hex
    applied_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    applied_by TEXT NOT NULL                 -- version stamp of the binary that applied the migration
);

-- palai_app never writes this journal (the runner does, as owner); it gets read-only visibility so a
-- doctor/health probe can surface the head. The REVOKE is the load-bearing part and it is self-re-
-- asserting, exactly like usage_ledger (000032): 000001's and 000029's blanket `GRANT ... ON ALL TABLES`
-- re-run on every boot and — now that schema_revisions EXISTS — re-hand palai_app UPDATE/DELETE on it. A
-- journal the process can restate or erase is not a journal. 000033 runs AFTER both blanket grants in the
-- chain (33 > 29 > 1) and no later migration re-grants this table, so this REVOKE re-asserts every boot.
GRANT SELECT ON schema_revisions TO palai_app;
REVOKE UPDATE, DELETE ON schema_revisions FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (33) ON CONFLICT DO NOTHING;
