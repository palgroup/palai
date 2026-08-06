-- Tenancy provisioning (E13 Task 2, TEN-003/MCI-001). The write half of the identity tables 000001
-- declared, minus the one A.2 Task 6 removed: projects, principals, api_keys. Every read/write here runs
-- under a tenant scope internal/identity publishes — PROJECT creation under the system scope (it
-- establishes a tenant, like bootstrap), everything else under the caller's own project (never a body
-- field, §39.2). The api_keys.key_hash is the only stored verifier; the bearer value never reaches a row.
--
-- FOUR STATEMENTS LEFT WITH THE organizations TABLE, and each had zero Go callers when it went:
-- InsertOrganization, ListOrganizations, GetOrganization (their route, POST/GET /v1/organizations, was
-- unmounted in Task 5) and OrganizationForProject, whose last two callers were deleted with the tagged
-- sweep. A statement whose table no longer exists is not dead weight, it is a `storage.Query` panic
-- waiting for the first caller to be written back.

-- name: InsertPrincipal
INSERT INTO principals (id, project_id, kind) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: InsertProject
-- ‼️ THE POLICY IS WRITTEN AT BIRTH, AND IT USED TO BE LEFT NULL. A project created through this
-- statement had no `default_tools`, so its agents were offered NOTHING — and a model with no tools does
-- not refuse, it NARRATES. Measured 2026-08-06 on a project Palai Cloud had just opened for a customer:
-- asked to list the repository's Swift files with the shell tool, the run made ZERO tool calls, reached
-- `completed`, and answered "I will use the shell tool to list the Swift files. Executing the command
-- now. ```bash ls *.swift``` Please hold on while I gather the res…". Every layer reported success.
--
-- Only `palai up` granted the baseline, at the CLI, to the one project it had made — so a self-hosted
-- operator got a working agent and every project opened over the API (which is the only way Cloud opens
-- one) got a mute one. The grant belongs where a tenant is born, not in one of the two paths that reach
-- the birth: `seedTenant` now passes toolset.Default(), and the CLI's own grant stands down when a
-- policy already names tools, which after this it always does.
INSERT INTO projects (id, display_name, config_policy) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: InsertAPIKey
INSERT INTO api_keys (id, project_id, principal_id, key_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING;

-- ListProjects returns every project the caller's scope admits (the projects policy is keyed on the
-- project's own id: the projects policy compares `id` to palai.project_id).
-- name: ListProjects
SELECT id, display_name, config_policy, created_at
FROM projects
ORDER BY created_at, id;

-- name: GetProject
SELECT id, display_name, config_policy, created_at
FROM projects
WHERE id = $1;

-- UpdateProjectConfigPolicy writes the §14 project-layer policy the resolver reads. RLS scopes the row to
-- the caller's OWN project — the projects policy compares `id` to palai.project_id — so a
-- foreign/unknown id updates zero rows (rendered NotFound, no oracle). That is narrower than the
-- organization-wide reach this said before A.2: a caller can no longer write a sibling project's policy,
-- because there is no sibling relation left to be inside of.
-- name: UpdateProjectConfigPolicy
UPDATE projects SET config_policy = $2, updated_at = clock_timestamp()
WHERE id = $1
RETURNING id;

-- ListAPIKeys returns key METADATA only — never key_hash. Under RLS this lists every key whose
-- project_id is the caller's own (the api_keys `tenant_isolation` policy); a system-scoped caller sees all of
-- them, which is the one path that still reaches across projects.
-- name: ListAPIKeys
SELECT id, project_id, principal_id, scopes, expires_at, created_at, revoked_at
FROM api_keys
ORDER BY created_at, id;

-- name: GetAPIKey
SELECT id, project_id, principal_id, scopes, expires_at, created_at, revoked_at
FROM api_keys
WHERE id = $1;

-- RevokeAPIKey is idempotent: an already-revoked key keeps its first revoked_at. A foreign/unknown id is
-- invisible under RLS and returns no row (NotFound).
-- name: RevokeAPIKey
UPDATE api_keys SET revoked_at = coalesce(revoked_at, clock_timestamp())
WHERE id = $1
RETURNING id;

-- ProjectExists confirms the target project is the caller's OWN before a key is minted for it (a foreign
-- project is invisible under RLS, so this returns no row). It used to confirm shared ORGANIZATION
-- membership; with organizations gone the two questions have collapsed into one, and this statement is
-- the same shape for a different reason.
-- name: ProjectExists
SELECT 1 FROM projects WHERE id = $1;

