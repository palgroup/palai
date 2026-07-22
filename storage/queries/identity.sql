-- Identity resolution (spec §39.2). This is the one read keyed by the credential
-- hash rather than the tenant: it establishes the tenant every later query is
-- scoped by. The stored verifier is a hash; the full key is never persisted.
-- A revoked OR expired key (E13 T2: api_keys.expires_at) matches no row, so both are
-- indistinguishable from an unknown key — resolved as invalid_token, no oracle. scopes
-- (E13 T2) carries the key's coarse capability set (empty = unrestricted).

-- name: VerifyAPIKey
SELECT organization_id, project_id, principal_id, scopes
FROM api_keys
WHERE key_hash = $1 AND revoked_at IS NULL
  AND (expires_at IS NULL OR expires_at > now());
