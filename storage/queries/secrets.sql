-- Secret-ref store (E13 Task 3, SEC-002/MCI-002). The write half of secret_refs: the DB-backed secret store
-- the resolver chain puts in front of the env-file bridge. The stored `ciphertext` is a master-key
-- AES-256-GCM sealed blob; the plaintext value never reaches a row, and the metadata queries never select
-- the ciphertext.
--
-- EVERY STATEMENT HERE IS PROJECT-KEYED, AND UNTIL 000006 NONE OF THEM WAS. A.2 dropped this table's
-- organization_id without adding a project_id, so 000002 could only give it
-- `palai_apply_installation_policy` — a policy that admits any connection declaring any scope. A secret
-- NAME was single-occupancy across the installation and every project resolved the same row. 000006 adds
-- project_id and widens the uniqueness to (project_id, name, version) -- IN PLACE, under the baseline's
-- own constraint name `secret_refs_name_version_key`, because 000001 re-adds that constraint on every
-- boot under a guard that tests only the NAME. That migration's header carries the whole reason.
--
-- TWO SCOPES ARE VISIBLE TO A CALLER, AND THE DIFFERENCE IS THIS FILE'S PRECEDENCE RULE:
--
--   project_id = <caller's project>   a secret this project owns.
--   project_id = ''                   a row written BEFORE 000006, when no project could be recorded.
--                                     000006 assigns none of them — nothing in the schema records who
--                                     wrote them, and two names are referenced by two projects each — so
--                                     they stay readable to every scope. That is the pre-000006 behaviour,
--                                     preserved deliberately: applying the migration must not stop the 104
--                                     names a running installation already holds from resolving.
--
-- Every read below names BOTH and orders `(project_id = $1) DESC`, so the caller's OWN row wins. The RLS
-- policy admits the same two sets; the explicit predicate is not redundant with it but the second half of
-- a pair — this tree has shipped a boundary that existed on only one of two layers.
--
-- HONEST CEILING: the '' rows are a leak this file does not close. Two projects sharing a legacy name still
-- read the same bytes. Closing it needs an operator claiming each name into a project, and no route does
-- that yet (palai-cloud/docs/devredilen-isler.md).

-- NextSecretVersion computes the next version for a name WITHIN ONE PROJECT: 1 for a fresh name, or
-- MAX(version)+1 for a rotation. A returned 1 means the name had no prior version — the store renders a
-- rotate of such a name as NotFound (a rotation implies an existing secret).
--
-- IT DOES NOT SEE THE '' SCOPE, WHICH IS AN ANSWER RATHER THAN AN OVERSIGHT. Two consequences, both stated
-- because a reader will meet one of them:
--   - a project writing a name that exists only as a legacy '' row starts at version 1, not at the legacy
--     row's next number. It is a DIFFERENT secret — different owner, different constraint row — and
--     ResolveSecretRef's ordering makes the project's own version 1 outrank the legacy version 3.
--   - ROTATING a name the project does not own is therefore a 404. That is the boundary working: a project
--     cannot append a version to another scope's secret. The operator creates it under the project instead.
-- name: NextSecretVersion
SELECT coalesce(max(version), 0) + 1 FROM secret_refs WHERE project_id = $1 AND name = $2;

-- name: InsertSecretRef
INSERT INTO secret_refs (id, project_id, name, version, ciphertext) VALUES ($1, $2, $3, $4, $5)
RETURNING created_at;

-- ResolveSecretRef returns the effective version's ciphertext for a name — the bytes the resolver chain
-- decrypts. Its ONE caller is identity.SecretStore.Resolve, which runs it under the caller's own tenant.
--
-- THE ORDER BY IS THE PRECEDENCE RULE AND BOTH KEYS ARE LOAD-BEARING:
--   (project_id = $1) DESC   the project's OWN secret beats a legacy '' row of the same name. Without it a
--                            legacy row at a higher version would shadow the secret the project just
--                            wrote, and the migration would silently outrank every new write.
--   version DESC             the latest version within the winning scope (the SEC-002 rotation property).
-- The pair is total: `UNIQUE (project_id, name, version)` allows at most one row per (scope, version), so
-- LIMIT 1 picks a determined row rather than an arbitrary one.
--
-- A name in NEITHER scope returns no row — the clean miss the resolver treats as "fall back to the env
-- bridge", and the only miss this statement had before 000006. A name owned by ANOTHER project now returns
-- no row as well, and that second miss IS the boundary 000006 restored.
-- name: ResolveSecretRef
SELECT ciphertext FROM secret_refs
WHERE name = $2 AND project_id IN ($1, '')
ORDER BY (project_id = $1) DESC, version DESC
LIMIT 1;

-- ListSecretRefs returns metadata ONLY (name, effective version, that version's created_at as updated_at) —
-- never the ciphertext. One row per NAME under ResolveSecretRef's precedence, so the list and the read
-- cannot disagree about which secret a name means: a name held by BOTH the caller's project and the legacy
-- scope is listed once, at the project's own version.
--
-- The set is the caller's project plus the legacy scope. A foreign project's secret is invisible, and since
-- 000006 that is a refusal rather than a sentence describing one.
-- name: ListSecretRefs
SELECT name, version, updated_at FROM (
    SELECT DISTINCT ON (name) name, version, created_at AS updated_at
    FROM secret_refs
    WHERE project_id IN ($1, '')
    ORDER BY name, (project_id = $1) DESC, version DESC
) effective
ORDER BY name;

-- GetSecretRef returns one name's metadata under the same precedence. TWO names return no row and the API
-- renders both as 404: one this installation does not hold, and one held by ANOTHER project. Conflating
-- them is deliberate — distinguishing them would tell an outsider that a name exists somewhere else.
-- name: GetSecretRef
SELECT DISTINCT ON (name) name, version, created_at AS updated_at
FROM secret_refs
WHERE name = $2 AND project_id IN ($1, '')
ORDER BY name, (project_id = $1) DESC, version DESC;
