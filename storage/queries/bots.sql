-- The kind-agnostic bot registry (migration 000061, spec plan 2026-08-03 §Task 4). Every statement below
-- treats `config` as an opaque byte string: none of them inspect its interior, and none of them names a
-- channel. That is not a convention this file follows — it is the property the table exists to hold.

-- name: InsertBot
INSERT INTO integration_bots
    (id, organization_id, project_id, name, kind, agent_revision_id, repository_binding_id, principal_id, config)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING created_at;

-- name: GetBot
SELECT id, name, kind, agent_revision_id, repository_binding_id, principal_id, config, disabled, created_at
FROM integration_bots
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- ListBots pages a project's bots newest-first (the shared keyset envelope). Tenant-scoped by RLS; the
-- org/project predicate is defence-in-depth. ORDER BY is not optional here — this tree has twice decided
-- a security outcome, and once a false-red gate, on a LIMIT with no ORDER BY behind it.
-- name: ListBots
SELECT id, name, kind, agent_revision_id, repository_binding_id, principal_id, config, disabled, created_at
FROM integration_bots
WHERE organization_id = $1 AND project_id = $2
  AND ($3::timestamptz IS NULL OR created_at >= $3)
  AND ($4::timestamptz IS NULL OR created_at <= $4)
  AND ($5::timestamptz IS NULL OR (created_at, id) < ($5, $6))
ORDER BY created_at DESC, id DESC
LIMIT $7;

-- UpdateBot applies a partial revision: a NULL parameter COALESCEs to the stored value, so a field the
-- caller did not mention is left exactly as it was rather than overwritten with a zero value. `kind` is
-- absent from the SET list on purpose — it is this row's identity, immutable for the same reason
-- slack_connections.team_id is (queries/slack.sql): changing what a bot IS is a new registration, not a
-- revision of this one.
-- name: UpdateBot
UPDATE integration_bots
SET name = COALESCE($4, name),
    agent_revision_id = COALESCE($5, agent_revision_id),
    repository_binding_id = COALESCE($6, repository_binding_id),
    principal_id = COALESCE($7, principal_id),
    config = COALESCE($8, config),
    disabled = COALESCE($9, disabled)
WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING id;

-- name: DeleteBot
DELETE FROM integration_bots
WHERE id = $1 AND organization_id = $2 AND project_id = $3
RETURNING id;
