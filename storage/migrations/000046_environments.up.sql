-- 000046 (E25 T3): AN ENVIRONMENT — a named group of key→value pairs an agent's shell receives.
--
-- E25's ONE migration. Three statements, and the reason each exists was measured against this tree
-- rather than assumed (plan §1.4).
--
-- WHAT IS DELIBERATELY NOT HERE: A SECOND ENCRYPTED-VALUE TABLE. The values live in secret_refs
-- (000031) and nothing else. A second ciphertext column would have to re-earn three properties that
-- already work here: AES-256-GCM envelope sealing under the one master key
-- (internal/identity/secrets.go), `GRANT SELECT, INSERT` plus a REVOKE UPDATE, DELETE that
-- re-asserts itself on every boot (000031:38-45), and the fact that the ONLY query touching
-- `ciphertext` is unreachable from any /v1 route (storage/queries/secrets.sql). A tree that writes a
-- secret path a third time writes it wrong a third time. The secret NAME is derived rather than
-- stored: `env:<environment_id>:<key>`.
--
-- ORG-SCOPED, NOT PROJECT-SCOPED, and this is a consequence rather than a preference. 000031:16 says
-- verbatim: "secret_refs carries organization_id (NO project_id: it fronts the org-scoped env
-- bridge)". An environment whose grouping row were project-scoped while its values were org-scoped
-- would let one project resolve another project's key by deriving the same secret name. So both
-- tables below are org-scoped and the product consequence is stated on the console screen: two
-- projects in one organization see the same environments.

-- ---------------------------------------------------------------------------------------------------
-- (1) environments — the GROUPING IDENTITY.
-- ---------------------------------------------------------------------------------------------------
--
-- WHY A TABLE AND NOT A NAME CONVENTION. A prefix convention (only `env:<name>:<KEY>` secret names,
-- no table) cannot do three things the operator's own flow needs: an environment must be creatable
-- with NO keys yet (create the environment, then fill it), it must be LISTABLE (a convention makes
-- that a DISTINCT split_part over secret names), and an agent revision must be able to reference one
-- with referential integrity (a convention lets a revision name an environment that never existed).
CREATE TABLE IF NOT EXISTS environments (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    name TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- One environment per name per organization. The name is what an operator types and what the
    -- console lists; two rows claiming it would make "the production environment" ambiguous, and this
    -- tree has twice decided a security outcome on an ambiguous lookup resolved by an unordered
    -- LIMIT 1 (E19 T1 Slack registration, E23 tool lookup).
    UNIQUE (organization_id, name)
);

-- ---------------------------------------------------------------------------------------------------
-- (2) environment_values — MEMBERSHIP. NOT ciphertext, and the name says so.
-- ---------------------------------------------------------------------------------------------------
--
-- A row here says "this environment has a key by this name". The BYTES are one secret_refs version
-- addressed by the derived name. So this table holds no credential and its whole content is safe to
-- return from a read route — which is what makes `GET /v1/environments/{id}` able to carry key NAMES,
-- versions and update times while carrying no value.
--
-- organization_id is carried EXPLICITLY even though environment_id already reaches it through
-- environments. Two reasons, and the first is not stylistic: palai_apply_tenant_policy's org form
-- needs the column ON THE TABLE (it builds `organization_id = current_setting('palai.org_id')`), and
-- the alternative — a child policy resolving the parent's org through an EXISTS subquery, the
-- delivery_attempts/job_attempts shape in 000029 — is the form used only where the child genuinely
-- cannot carry a tenant column. Second, the static guard in storage/migrations_test.go keys on
-- `organization_id` appearing in the CREATE TABLE body, so a child table without it silently leaves
-- that guard's count unchanged.
--
-- PLAN CORRECTION: §T3 lists this table's columns as `(environment_id, key, created_at)`. That shape
-- cannot carry the org-scoped tenant policy the SAME section requires two lines later. The column is
-- added; the plan's tenancy requirement is what decided it.
CREATE TABLE IF NOT EXISTS environment_values (
    environment_id TEXT NOT NULL REFERENCES environments (id) ON DELETE CASCADE,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    key TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (environment_id, key)
);

-- ---------------------------------------------------------------------------------------------------
-- (3) agent_revisions.environment — THE BOND.
-- ---------------------------------------------------------------------------------------------------
--
-- WHY THIS IS AN ALTER AND NOT A JSONB FIELD. agent_revisions carries TYPED columns and has no
-- free-form config JSON, and 000019_agents.up.sql:34-37 states why in its own words: "The E12 fields
-- (mcp/skills/hooks/knowledge) are deliberately ABSENT — dead config is not stored (honest naming)".
-- That comment is also a warning, and it is obeyed here: this column lands in the SAME change as the
-- orchestrator code that reads it and the routes that write it. A column nothing reads would break
-- the table's own rule.
--
-- NOT AN FK, and the reason is the DEFAULT. Every existing revision in every deployment has no
-- environment, and the honest representation of that is the empty string rather than a NULL that
-- would make `environment IS NULL` and `environment = ''` two different kinds of nothing. An FK
-- cannot accept ''. The referential check therefore lives in the write path: a revision naming an
-- environment that does not exist is refused at CREATE and again at PUBLISH
-- (internal/automation/agents.go).
ALTER TABLE agent_revisions ADD COLUMN IF NOT EXISTS environment TEXT NOT NULL DEFAULT '';

-- ---------------------------------------------------------------------------------------------------
-- Tenancy: two tables, two policies, two grants — and one grant is deliberately WIDER than 000031's.
-- ---------------------------------------------------------------------------------------------------
--
-- has_project=false on both: org-scoped, matching secret_refs, for the reason in the header.
CALL palai_apply_tenant_policy('environments', 'organization_id', false);
CALL palai_apply_tenant_policy('environment_values', 'organization_id', false);

-- Both tables are created AFTER 000001's and 000029's blanket `GRANT ... ON ALL TABLES`, which re-run
-- every boot but ran BEFORE this file on the boot that creates them, so each needs its own grant or
-- the runtime role fails closed with "permission denied for table environments" instead of with the
-- row-scoped policy.
GRANT SELECT, INSERT ON environments TO palai_app;

-- environment_values GETS DELETE, AND THAT IS THE WHOLE POINT OF SPLITTING MEMBERSHIP FROM BYTES.
--
-- A row can never be deleted from secret_refs: 000031:45 REVOKEs UPDATE and DELETE and re-asserts the
-- REVOKE on every boot, on purpose — the version history is retained for audit. So "remove the
-- JIRA_TOKEN key from the production environment" cannot mean "delete the bytes", and a button that
-- claimed to delete something it did not delete would be worse than no button.
--
-- What it CAN mean, exactly, is: remove the BINDING. The membership row goes; the sealed versions stay
-- in secret_refs, unreachable because nothing names them any more (the derived name
-- `env:<environment_id>:<key>` is only ever constructed from a membership row). The console says this
-- in those words at the confirmation step, and docs/operations/environments.md says it again.
--
-- UPDATE is withheld: a key's name is its identity here, and "rename" would silently orphan the
-- versions stored under the old derived name. Renaming is remove-then-add, which is honest about the
-- new key starting at version 1.
GRANT SELECT, INSERT, DELETE ON environment_values TO palai_app;
REVOKE UPDATE ON environment_values FROM palai_app;

-- Neither REVOKE below is decoration, and the 000031 precedent states the mechanism: main.go re-runs
-- the WHOLE chain on every boot, so on boot #2 both 000001's and 000029's blanket
-- `GRANT ... ON ALL TABLES` re-run and — now that these tables EXIST — re-hand palai_app UPDATE and
-- DELETE on both. This file runs LAST in the chain (46 > 29 > 1) and no later migration re-grants
-- them, so these REVOKEs re-assert after them every boot.
--
-- environments is append-only for the same reason its values are: an environment id is embedded in
-- every derived secret name, so deleting or renaming one would orphan every version it grouped. This
-- is a measured ceiling rather than a permanent design: `DELETE FROM environments` is not a feature
-- E25 ships, and the console offers no such button, so the honest posture is to withhold the grant
-- rather than to build a cascade whose secret_refs half cannot cascade.
REVOKE UPDATE, DELETE ON environments FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (46) ON CONFLICT DO NOTHING;
