-- Host quarantine + snapshot byte-archive rider + merge_records parent_run_id index (spec §29.7-29.10,
-- E10 Task 6). Three concerns land together because they are the durable state the T6 recovery paths
-- add on top of 000008's workspace substrate:
--
--  1. host_quarantine: a durable set of hosts a destroy-failure has poisoned (spec §29 SAN-008). In the
--     local tier a "host" is a provision-root / runner identity — there is no hosts/runners registry
--     (enrollment is cert-based, runner_gateway.go), so the quarantine IS the registry: placement of a
--     NEW allocation consults it and refuses a quarantined host, while a run already executing there is
--     untouched.
--  2. workspace_snapshots.object_key + archive_checksum + size_bytes: E09 recorded a MANIFEST-only
--     snapshot (checksums, no bytes). T6 tars the allocation and PUTs it to the object store; these
--     columns record WHERE the bytes live and how to verify them, exactly as checkpoints.object_key does.
--     DEFAULT '' keeps every existing manifest-only row valid (an empty key means no archived bytes).
--  3. merge_records(parent_run_id) index: the E09 Task 6 M3 deferral (recorded in the E10 plan §7 #8) —
--     the parent_run_id FK carried no index, so the parent's merge lookups scan. Added here as the rider
--     the plan assigned to this migration.
--
-- Every CREATE / ADD COLUMN IF NOT EXISTS keeps the migration idempotent (Migrate is re-run per boot),
-- matching the 000008/000015/000016 pattern. Tenant scope is the composite (organization_id, project_id)
-- FK every execution row carries (spec §39.2).

CREATE TABLE IF NOT EXISTS host_quarantine (
    -- The poisoned host's identity. In the local tier this is the provision-root / runner identity a
    -- destroy failed under (spec §29 SAN-008). PK: a host is quarantined at most once; a repeat
    -- failure re-quarantines idempotently (ON CONFLICT DO NOTHING at the write).
    host_id TEXT PRIMARY KEY,
    -- Why it was quarantined (a destroy failure's typed reason), so the doctor can show it and an
    -- operator can act. Free text, honest-empty when the caller has no detail.
    reason TEXT NOT NULL DEFAULT '',
    quarantined_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp()
);

-- The snapshot byte-archive rider (spec §29.10, SAN-005 restore). A manifest-only row (E09) keeps the
-- '' defaults; a T6 snapshot with archived bytes records the object key, the archive's own content
-- checksum, and its size. The manifest checksums (tree/index/file) stay the create-side integrity the
-- restore re-derives EQUAL; these three describe the transport artifact, not the tree.
ALTER TABLE workspace_snapshots
    ADD COLUMN IF NOT EXISTS object_key TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_snapshots
    ADD COLUMN IF NOT EXISTS archive_checksum TEXT NOT NULL DEFAULT '';
ALTER TABLE workspace_snapshots
    ADD COLUMN IF NOT EXISTS size_bytes BIGINT NOT NULL DEFAULT 0;

-- The E09 Task 6 M3 deferral: index the parent_run_id FK so a parent's merge lookups are index-backed,
-- not a scan (recorded in the E10 plan §7 #8 as this migration's rider).
CREATE INDEX IF NOT EXISTS merge_records_by_parent_run ON merge_records (parent_run_id);

-- host_quarantine is DML-granted to the application role explicitly (000001's blanket GRANT covered only
-- the tables that existed then; a security boundary, so it is explicit here, not inherited).
GRANT SELECT, INSERT, DELETE ON host_quarantine TO palai_app;

INSERT INTO schema_migrations (version) VALUES (17) ON CONFLICT DO NOTHING;
