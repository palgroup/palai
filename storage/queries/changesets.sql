-- Changeset compilation + persistence queries (spec §30.6-30.7, REP-005). The migration owns the
-- constraints; these are the read paths the compiler projects from and the write paths it records
-- through. Every query is tenant-scoped: without organization and project a read returns no row.

-- RunToolCalls reads a run's completed tool-call ledger — the authoritative record of what the run
-- did, independent of the model's prose (REP-005). The changeset compiler filters it for file-tool
-- writes (the changed-file set) and shell-tool calls (the checks transcript). Ordered chronologically
-- so a file written twice resolves to its latest content.
-- name: RunToolCalls
SELECT id, name, arguments::text, coalesce(result::text, '')
FROM tool_calls
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY created_at, id;

-- RunBaseCommit reads the run's latest preparation receipt base commit (spec §30.3, the
-- model-independent provenance). It is the base the changeset diffs the working tree against; a run
-- with no prepared repository returns no row and compiles no changeset.
-- name: RunBaseCommit
SELECT base_commit
FROM preparation_receipts
WHERE run_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY prepared_at DESC, id DESC
LIMIT 1;

-- InsertChangeset records an immutable changeset (spec §30.6). content_hash is its content address;
-- there is no UPDATE — a re-compile of the same ledger produces the same hash, so the row is written
-- once. patch/test-log artifact ids are nullable (a changeset with no diff or no checks has none).
-- name: InsertChangeset
INSERT INTO changesets
    (id, organization_id, project_id, run_id, base_commit, final_commit, final_tree, files,
     patch_artifact_id, test_log_artifact_id, patch_truncated, content_hash)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
ON CONFLICT (id) DO NOTHING;

-- InsertChangesetFinding records one likely-committed-secret (or license) finding over a file entering
-- the changeset (spec §30.4 committed-secret detection, §30.6 findings).
-- name: InsertChangesetFinding
INSERT INTO changeset_findings (id, changeset_id, organization_id, project_id, kind, path, rule)
VALUES ($1, $2, $3, $4, $5, $6, $7)
ON CONFLICT (id) DO NOTHING;
