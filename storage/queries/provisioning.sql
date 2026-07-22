-- Tenancy provisioning (E13 Task 2, TEN-003/MCI-001). The write half of the identity tables 000001
-- declared: organizations, projects, principals, api_keys. Every read/write here runs under a tenant
-- scope internal/identity publishes — organization creation under the system scope (it establishes a
-- tenant, like bootstrap), everything else under the caller's own organization (never a body field,
-- §39.2). The api_keys.key_hash is the only stored verifier; the bearer value never reaches a row.

-- name: InsertOrganization
INSERT INTO organizations (id, display_name) VALUES ($1, $2) ON CONFLICT DO NOTHING;

-- name: InsertPrincipal
INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, $4) ON CONFLICT DO NOTHING;

-- name: InsertProject
INSERT INTO projects (id, organization_id, display_name) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING;

-- name: InsertAPIKey
INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash, scopes, expires_at)
VALUES ($1, $2, $3, $4, $5, $6, $7) ON CONFLICT DO NOTHING;

-- ListOrganizations returns the organizations visible in the caller's scope. Under RLS this is the
-- caller's own organization (the organizations policy is id = palai.org_id); no cross-tenant listing.
-- name: ListOrganizations
SELECT id, display_name, created_at
FROM organizations
ORDER BY created_at, id;

-- name: GetOrganization
SELECT id, display_name, created_at
FROM organizations
WHERE id = $1;

-- ListProjects returns every project in the caller's organization (the projects policy is org-only).
-- name: ListProjects
SELECT id, organization_id, display_name, config_policy, created_at
FROM projects
ORDER BY created_at, id;

-- name: GetProject
SELECT id, organization_id, display_name, config_policy, created_at
FROM projects
WHERE id = $1;

-- UpdateProjectConfigPolicy writes the §14 project-layer policy the resolver reads. RLS scopes the row to
-- the caller's organization, so a foreign/unknown id updates zero rows (rendered NotFound, no oracle).
-- name: UpdateProjectConfigPolicy
UPDATE projects SET config_policy = $2, updated_at = clock_timestamp()
WHERE id = $1
RETURNING id;

-- ListAPIKeys returns key METADATA only — never key_hash. Under the org-wide provisioning scope this
-- lists every key in the caller's organization.
-- name: ListAPIKeys
SELECT id, organization_id, project_id, principal_id, scopes, expires_at, created_at, revoked_at
FROM api_keys
ORDER BY created_at, id;

-- name: GetAPIKey
SELECT id, organization_id, project_id, principal_id, scopes, expires_at, created_at, revoked_at
FROM api_keys
WHERE id = $1;

-- RevokeAPIKey is idempotent: an already-revoked key keeps its first revoked_at. A foreign/unknown id is
-- invisible under RLS and returns no row (NotFound).
-- name: RevokeAPIKey
UPDATE api_keys SET revoked_at = coalesce(revoked_at, clock_timestamp())
WHERE id = $1
RETURNING id;

-- ProjectExists confirms a target project belongs to the caller's organization before a key is minted for
-- it (a foreign project is invisible under RLS, so this returns no row).
-- name: ProjectExists
SELECT 1 FROM projects WHERE id = $1;
