-- Secret-ref store (E13 Task 3, SEC-002/MCI-002). The write half of migration 000031's secret_refs table:
-- the DB-backed secret store the resolver chain puts in front of the env-file bridge. secret_refs HAS NO
-- TENANT COLUMN, and since migration 000066 no tenant boundary either: its policy admits any connection
-- that declared a scope, so a secret NAME IS INSTALLATION-WIDE and UNIQUE (name, version) is too. A query
-- names only `name` because there is no other column to name. The stored `ciphertext` is a master-key
-- AES-256-GCM sealed blob; the plaintext value never reaches a row, and the metadata queries never select
-- the ciphertext.

-- NextSecretVersion computes the next version for a name: 1 for a fresh name, or
-- MAX(version)+1 for a rotation. A returned 1 means the name had no prior version — the store renders a
-- rotate of such a name as NotFound (a rotation implies an existing secret).
-- name: NextSecretVersion
SELECT coalesce(max(version), 0) + 1 FROM secret_refs WHERE name = $1;

-- name: InsertSecretRef
INSERT INTO secret_refs (id, name, version, ciphertext) VALUES ($1, $2, $3, $4)
RETURNING created_at;

-- ResolveSecretRef returns the latest version's ciphertext for a name, the bytes the resolver chain
-- decrypts. Its ONE caller is identity.SecretStore.Resolve, which runs it under WithInstallationScope —
-- so there is no org to enforce and no FOREIGN name to hide, per the header: 000066 made a name
-- installation-wide. An UNKNOWN name returns no row (a clean miss the resolver treats as "fall back to
-- the env bridge"), and that is the only miss this statement has.
-- name: ResolveSecretRef
SELECT ciphertext FROM secret_refs WHERE name = $1 ORDER BY version DESC LIMIT 1;

-- ListSecretRefs returns metadata ONLY (name, latest version, latest version's created_at as updated_at) —
-- never the ciphertext. One row per name in the INSTALLATION, for EVERY caller: identity.SecretStore
-- runs this under provisioningScope, but 000066's policy on secret_refs admits any connection that
-- declared a scope, so a tenant admin and the platform see the identical set. That is 000066's stated
-- ceiling, not an oversight here.
-- name: ListSecretRefs
SELECT name, max(version) AS version, max(created_at) AS updated_at
FROM secret_refs
GROUP BY name
ORDER BY name;

-- GetSecretRef returns one name's metadata. There is no FOREIGN name to refuse — see ListSecretRefs —
-- so an ABSENT name is the only one that returns no row.
-- name: GetSecretRef
SELECT name, max(version) AS version, max(created_at) AS updated_at
FROM secret_refs
WHERE name = $1
GROUP BY name;
