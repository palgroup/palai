-- Secret-ref store (E13 Task 3, SEC-002/MCI-002). The write half of migration 000031's secret_refs table:
-- the DB-backed secret store the resolver chain puts in front of the env-file bridge. Every read/write
-- runs under the caller's org scope (internal/identity), so RLS isolates one tenant's secrets from
-- another's — a query names only `name`, and organization_id is enforced by the tenant policy, never a
-- WHERE clause here. The stored `ciphertext` is a master-key AES-256-GCM sealed blob; the plaintext value
-- never reaches a row, and the metadata queries never select the ciphertext.

-- NextSecretVersion computes the next version for a name (org enforced by RLS): 1 for a fresh name, or
-- MAX(version)+1 for a rotation. A returned 1 means the name had no prior version — the store renders a
-- rotate of such a name as NotFound (a rotation implies an existing secret).
-- name: NextSecretVersion
SELECT coalesce(max(version), 0) + 1 FROM secret_refs WHERE name = $1;

-- name: InsertSecretRef
INSERT INTO secret_refs (id, organization_id, name, version, ciphertext) VALUES ($1, $2, $3, $4, $5)
RETURNING created_at;

-- ResolveSecretRef returns the latest version's ciphertext for a name (org enforced by RLS), the bytes the
-- resolver chain decrypts. A foreign/unknown name is invisible under RLS and returns no row (a clean miss
-- the resolver treats as "fall back to the env bridge").
-- name: ResolveSecretRef
SELECT ciphertext FROM secret_refs WHERE name = $1 ORDER BY version DESC LIMIT 1;

-- ListSecretRefs returns metadata ONLY (name, latest version, latest version's created_at as updated_at) —
-- never the ciphertext. One row per name in the caller's organization.
-- name: ListSecretRefs
SELECT name, max(version) AS version, max(created_at) AS updated_at
FROM secret_refs
GROUP BY name
ORDER BY name;

-- GetSecretRef returns one name's metadata (org enforced by RLS); an absent/foreign name returns no row.
-- name: GetSecretRef
SELECT name, max(version) AS version, max(created_at) AS updated_at
FROM secret_refs
WHERE name = $1
GROUP BY name;
