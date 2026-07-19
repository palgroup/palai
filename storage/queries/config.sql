-- Session config revision queries (spec §9.3, §14). Every read is tenant-scoped: a session or
-- project from another tenant matches no row, so a cross-tenant id leaks nothing (§39.2).

-- GetProjectConfig reads a project's config policy JSON (allowed_models / allowed_tools /
-- default_tools). A NULL config_policy yields NULL, which the caller reads as unrestricted.
-- name: GetProjectConfig
SELECT config_policy
FROM projects
WHERE organization_id = $1 AND id = $2;

-- InsertConfigRevision records a session config revision at the boundary where it applied. The
-- sequence is the change_config command's applied_sequence, so the latest-by-sequence revision
-- is the session's effective config (spec §9.3, §14).
-- name: InsertConfigRevision
INSERT INTO config_revisions
    (id, organization_id, project_id, session_id, command_id, sequence, model, tools, snapshot_hash, immediate)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- LatestSessionConfig reads a session's most recent config revision — the effective override the
-- orchestrator routes a model step under. No revision (never changed) yields no row, so the step
-- falls back to the deployment/project defaults.
-- name: LatestSessionConfig
SELECT model, tools
FROM config_revisions
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3
ORDER BY sequence DESC
LIMIT 1;
