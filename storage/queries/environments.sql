-- Environment store (E25 T3). The write and read halves of the two tables `environments` and
-- `environment_values`: the grouping identity and the key MEMBERSHIP rows. (They arrived in the old
-- chain's 000046; the 2026-08-04 squash folded that file into 000001_core, so the number is not cited —
-- a reference to a migration this tree no longer has is a reference a reader cannot check.)
--
-- NOT ONE STATEMENT IN THIS FILE MENTIONS `ciphertext`, AND THAT IS A GUARDED PROPERTY RATHER THAN A
-- convention: storage/ciphertext_sweep_test.go counts the NAME of every query in storage/queries/*.sql
-- whose STATEMENT (not its prose) touches that column, and pins the list to exactly
-- {InsertSecretRef, ResolveSecretRef}. A query added here that reached for a value would fail that
-- sweep, and the failure is the decision — demonstrated by adding one and watching it name the query.
--
-- The values are secret_refs rows addressed by a DERIVED name: `env:<environment_id>:<key>`. Every read
-- route below returns names, versions and times; the value has no read-back path at all, exactly as
-- secret_refs has none (storage/queries/secrets.sql).
--
-- BOTH TABLES ARE TENANT-SCOPED SINCE 000006, AND NEITHER WAS BEFORE IT. A.2 dropped their
-- organization_id without adding a project_id, so 000002 could only give them
-- `palai_apply_installation_policy` and an environment was INSTALLATION-WIDE — every project read every
-- environment and every membership row. 000006 adds `environments.project_id` with
-- `UNIQUE (project_id, name)`, and `environment_values` resolves its PARENT's project through
-- `palai_apply_child_policy`: it holds no project column of its own, because its
-- `environment_values_environment_id_fkey -> environments(id) ON DELETE CASCADE` already answers the
-- question once, and a second copy would be a second source of truth.
--
-- WHAT THAT MEANS FOR THE STATEMENTS BELOW, in two branches rather than one sentence:
--   - the METADATA reads (ListEnvironments, GetEnvironment, EnvironmentExists, DeleteEnvironmentValue,
--     RunEnvironmentKeys) carry NO tenant predicate and lean on the policy alone. Before 000006 that was
--     a policy admitting everyone; now it is a boundary, so the 404 those routes describe is real.
--   - the two statements that touch a SECRET's version take the caller's project explicitly, because
--     secret_refs holds two visible scopes and a read that ignored the precedence would report a version
--     the resolver does not serve. See storage/queries/secrets.sql for the rule.
--
-- LEGACY ROWS: an environment written before 000006 carries project_id = '' and stays readable to every
-- scope — 000006 assigns none of them, for the reasons in its header. That is the same leak secrets.sql
-- records, and it is not closed here.

-- name: InsertEnvironment
INSERT INTO environments (id, project_id, name, description) VALUES ($1, $2, $3, $4)
RETURNING created_at;

-- ListEnvironments returns one row per environment with its KEY COUNT — the count rather than the keys,
-- because a list of twenty environments does not need eighty key names to be useful and the detail route
-- exists for that. The LEFT JOIN keeps a keyless environment in the list, which is the whole reason
-- environments is a table (an operator creates the environment first and fills it second).
-- name: ListEnvironments
SELECT e.id, e.name, e.description, e.created_at, count(v.key) AS key_count
FROM environments e
LEFT JOIN environment_values v ON v.environment_id = e.id
GROUP BY e.id, e.name, e.description, e.created_at
ORDER BY e.name;

-- GetEnvironment reads one environment's own row; an absent/foreign id returns no row (RLS makes the
-- two indistinguishable, which is the intended 404).
-- name: GetEnvironment
SELECT id, name, description, created_at FROM environments WHERE id = $1;

-- ListEnvironmentKeys is the detail route's payload and the sharpest projection in this file: the key
-- NAME, the secret's latest VERSION, and when that version was written. The version and the time come
-- from secret_refs — joined on the DERIVED name, which is why the join predicate is a concatenation
-- rather than a foreign key — and the ciphertext column is not in the select list, is not in the join
-- condition, and is not in the grouping.
--
-- The LEFT JOIN with a COALESCE'd version is deliberate rather than defensive: a membership row whose
-- secret version is missing is a state the write path makes impossible (both rows land in ONE
-- transaction), so version 0 would mean the database was edited by hand. Reporting 0 shows that
-- honestly instead of dropping the key from the list, which would make a hand-edit invisible.
--
-- IT WAS `max(s.version)` OVER A PLAIN JOIN UNTIL 000006, AND THAT NOW REPORTS THE WRONG NUMBER. Since
-- 000006 a caller sees secret_refs rows in two scopes — its own project and the legacy '' — and
-- ResolveSecretRef serves the PROJECT's row even when a legacy row carries a higher version. A `max` would
-- announce that legacy version while the agent's shell received the project's, which is a list disagreeing
-- with the read about which secret a key means. The lateral below is ResolveSecretRef's ordering, per key,
-- so the two cannot diverge. $1 is the caller's project, $2 the environment.
-- name: ListEnvironmentKeys
SELECT v.key, COALESCE(s.version, 0) AS version, s.updated_at
FROM environment_values v
LEFT JOIN LATERAL (
    SELECT r.version, r.created_at AS updated_at
    FROM secret_refs r
    WHERE r.name = 'env:' || v.environment_id || ':' || v.key
      AND r.project_id IN ($1, '')
    ORDER BY (r.project_id = $1) DESC, r.version DESC
    LIMIT 1
) s ON true
WHERE v.environment_id = $2
ORDER BY v.key;

-- UpsertEnvironmentValue records the MEMBERSHIP half of a create-or-rotate. It is ON CONFLICT DO
-- NOTHING rather than DO UPDATE because there is nothing to update: the row is (environment, key) and
-- created_at records when the key first joined the environment. A rotation is a new secret_refs
-- version, so the version history lives where the versions do, not here.
-- name: UpsertEnvironmentValue
INSERT INTO environment_values (environment_id, key) VALUES ($1, $2)
ON CONFLICT (environment_id, key) DO NOTHING;

-- DeleteEnvironmentValue removes the BINDING and nothing else. The sealed versions stay in secret_refs,
-- which grants no DELETE (000001_core.up.sql's `GRANT SELECT,INSERT ON TABLE secret_refs TO palai_app`, and
-- the whole chain re-asserts its grants on every boot); after this runs, nothing
-- names them, because the derived name is only ever built from a membership row. RETURNING key so the
-- caller can 404 a key the environment never had rather than reporting a success it did not achieve.
-- name: DeleteEnvironmentValue
DELETE FROM environment_values WHERE environment_id = $1 AND key = $2 RETURNING key;

-- EnvironmentExists is the referential check the agent-revision write path runs, at create and again at
-- publish. It exists because agent_revisions.environment is a TEXT column with DEFAULT '' rather than an
-- FK — '' is not a referencable id, and representing "no environment" as NULL would make
-- `environment IS NULL` and `environment = ''` two different kinds of nothing.
-- name: EnvironmentExists
SELECT 1 FROM environments WHERE id = $1;

-- RunEnvironmentKeys is what the ORCHESTRATOR reads at attempt start, and it returns KEY NAMES ONLY.
--
-- It walks the run's PINNED revision (never "the latest revision of that profile"), so an environment
-- key added after the run started does not appear and one removed after it started still does — the
-- AGT-001 old-run reproducibility rule, applied to an environment. Run templates are deliberately absent
-- from the COALESCE: run_template_revisions has no environment column, because a template is
-- profile-free and E25 binds an environment to an AGENT.
--
-- The derived secret NAME is returned alongside each key so the orchestrator does not rebuild it by
-- string concatenation at resolve time. One place builds it: this statement.
-- name: RunEnvironmentKeys
SELECT v.key, 'env:' || v.environment_id || ':' || v.key AS secret_name
FROM runs r
JOIN agent_revisions ar ON ar.id = r.agent_revision_id
JOIN environment_values v ON v.environment_id = ar.environment
WHERE r.id = $1 AND ar.environment <> ''
ORDER BY v.key;

