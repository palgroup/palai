-- Identity resolution (spec §39.2). This is the one read keyed by the credential
-- hash rather than the tenant: it establishes the tenant every later query is
-- scoped by. The stored verifier is a hash; the full key is never persisted.

-- name: VerifyAPIKey
SELECT organization_id, project_id, principal_id
FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL;
