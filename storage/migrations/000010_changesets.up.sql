-- Changeset + secret/license findings (spec §30.6-30.7, REP-005) and the richer artifact fields
-- (§22.6) their patch/test-log artifacts populate. A Changeset is a FIRST-CLASS, immutable summary
-- of what a coding run changed, compiled from the file-tool write ledger — NOT from the model's prose
-- (§30.6 line 3314, REP-005): base/final commit + tree, the added/modified file set, the patch and
-- test-log artifact references, and any likely-committed-secret findings. content_hash is the content
-- address that makes the row immutable (a re-compile of the same ledger yields the same hash; the LP
-- Task 11 content-addressing pattern) — there is no UPDATE path.
--
-- CREATE ... IF NOT EXISTS + ADD COLUMN IF NOT EXISTS keep the migration idempotent (Migrate re-runs
-- per boot; the 000008/000009/000011 pattern). Tenant scope is the composite (organization_id,
-- project_id) FK to projects every execution row carries (§39.2); the new tables are granted to
-- palai_app explicitly (000001's blanket GRANT covered only the tables that existed then).

CREATE TABLE IF NOT EXISTS changesets (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The authoring run whose file-tool ledger this changeset was compiled from (spec §30.6 "authoring
    -- run/tool lineage"). A changeset belongs to one run; provenance links live on the files JSONB.
    run_id TEXT NOT NULL REFERENCES runs (id),
    -- The preparation base commit and the final commit/tree the changeset spans (spec §30.6). base is
    -- the model-independent preparation receipt commit; final is HEAD after the run's commits (empty
    -- when nothing was committed — the working-tree patch still carries the change).
    base_commit TEXT NOT NULL DEFAULT '',
    final_commit TEXT NOT NULL DEFAULT '',
    final_tree TEXT NOT NULL DEFAULT '',
    -- The added/modified file set compiled from the file-tool write ledger: a JSONB array of
    -- {path, change, before_hash, after_hash, tool_call_id}. This is the load-bearing REP-005 record —
    -- derived from the tool_calls the run actually issued, so the model's summary cannot alter it.
    files JSONB NOT NULL DEFAULT '[]',
    -- The patch and test-log artifacts (spec §30.6): immutable object-store outputs the write-path
    -- committed (the artifacts row carries media/logical type + provenance). Nullable — a changeset
    -- with no diff or no checks has none.
    patch_artifact_id TEXT REFERENCES artifacts (id),
    test_log_artifact_id TEXT REFERENCES artifacts (id),
    -- The patch artifact was truncated at the size bound (spec §30.6 "truncation marker"): the stored
    -- object is a prefix, so a reader knows the diff is not complete.
    patch_truncated BOOLEAN NOT NULL DEFAULT false,
    -- The content address of the changeset (spec §30.6 immutable summary; LP Task 11 content_hash):
    -- a digest over base/final + the sorted file set + artifact keys + findings. Equal ledgers hash
    -- equal; there is no UPDATE path, so the row is immutable once written.
    content_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE IF NOT EXISTS changeset_findings (
    id TEXT PRIMARY KEY,
    changeset_id TEXT NOT NULL REFERENCES changesets (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- A likely-committed-secret (or, later, license) finding over the files entering the changeset
    -- (spec §30.4 committed-secret detection, deferred from preparation to here; §30.6 findings). kind
    -- distinguishes secret from license; rule names the matched shape; path is the changed file it hit.
    kind TEXT NOT NULL DEFAULT 'secret',
    path TEXT NOT NULL DEFAULT '',
    rule TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The richer §22.6 artifact fields, landing with their first producer (T5's patch/test-log artifacts).
-- 000001 wrote only the base columns (id/org/project/run/object_key/size/checksum) by design — no
-- column without a writer. media_type + logical_type classify the object (report/patch/diff/log/
-- test-result); malware_scan_status carries the scan outcome (no scanner is wired yet, so writers set
-- 'not_scanned' honestly); provenance links the artifact back to its changeset/run/tool lineage.
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS media_type TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS logical_type TEXT NOT NULL DEFAULT '';
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS malware_scan_status TEXT NOT NULL DEFAULT 'not_scanned';
ALTER TABLE artifacts ADD COLUMN IF NOT EXISTS provenance JSONB NOT NULL DEFAULT '{}';

GRANT SELECT, INSERT, UPDATE, DELETE ON changesets, changeset_findings TO palai_app;

INSERT INTO schema_migrations (version) VALUES (10) ON CONFLICT DO NOTHING;
