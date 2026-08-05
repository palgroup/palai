-- 000006 (project-scoped secrets and environments): the two boundaries A.2 REMOVED rather than moved come
-- back, keyed on the column everything else in this schema is keyed on.
--
-- A.2 carried `organization_id` to `project_id` on eighty-odd tables. On these three it did not: it dropped
-- the column and left the tables reachable by every scope in the installation. 000002's own catalogue says
-- so in words -- `palai_apply_installation_policy('secret_refs')` and the same for `environments` and
-- `environment_values` -- and storage/tenant.go's WithInstallationScope doc states the consequence: "on this
-- installation every project can read every environment, every environment value and every secret ref."
--
-- WHY NOW, MEASURED RATHER THAN ARGUED. It is not a hypothetical. Measured on this installation
-- 2026-08-05, immediately before this file, by attributing every secret name to the project-scoped rows
-- that reference it (the twelve referencing columns are enumerated in the NOTICE block below):
--
--     secret names total                                104
--     named by >= 1 project-scoped row                   19
--     named by MORE THAN ONE project                      2   <- github-conn, pm-openai
--     derived env:<environment_id>:<key> names            47
--     named by no project-scoped row at all               38
--     projects                                           62
--
-- Two names are already claimed by two different projects each, and today both projects resolve the SAME
-- bytes. That is the leak this file closes, and it is present in real data rather than predicted.
--
--
-- THE EXISTING ROWS ARE NOT ASSIGNED TO ANYONE, AND THAT IS A DECISION WITH A REASON.
--
-- A secret written installation-wide has no owner recoverable from the schema: nothing links a secret_ref
-- to a project. Back-attribution through the referencing columns answers for 19 of 104 names and is
-- AMBIGUOUS for two of those 19. `environments` has the same problem one level up -- the 47 derived names
-- trace to an environment, and an environment had no project either. And there is no default project to
-- fall back on: 000001 creates `projects` and seeds no row, so on a fresh install this file runs when the
-- table is empty, while on this installation it would have to choose among 62.
--
-- Binding a credential to the WRONG customer is worse than leaving it where it is. So every existing row
-- keeps `project_id = ''`, which is not an invented escape -- it is 000002's own written contract:
--
--     000002_row_level_security.up.sql:27-30
--     "a row whose project_id is '' is installation-global data written before any tenant existed, and it
--      stays readable to every scope"
--
-- HONEST CEILING, AND IT IS THE POINT OF THE PARAGRAPH RATHER THAN A FOOTNOTE: this boundary binds rows
-- written FROM NOW ON. The 104 names that already exist stay installation-wide, so for them the leak
-- measured above is NOT closed by this file. Two things would close it, and neither is in this migration:
-- an operator surface for claiming an existing secret into a project (no such route exists -- there is no
-- PATCH on /v1/secret-refs and no project field on the create body), and a decision about the two
-- ambiguous names, which no automation can make. Both are recorded in
-- palai-cloud/docs/devredilen-isler.md.
--
-- THE COUNTERPART GUARD IS IN GO, NOT HERE, and it is named so this file is not read as the whole fence:
-- identity.TestANewSecretCannotBeWrittenWithoutAProject asserts the write path can never PRODUCE a
-- `project_id = ''` row. A default is a value a writer can inherit by saying nothing; without that test
-- the empty string would be a hole the first new secret falls into rather than a record of the past.
--
--
-- `environment_values` GETS NO COLUMN OF ITS OWN, and that is this schema's existing idiom rather than a
-- shortcut. It carries `environment_values_environment_id_fkey -> environments(id) ON DELETE CASCADE` and
-- `environment_values_pkey (environment_id, key)`, so its project is its parent's, once. 000002 already
-- resolves four tables that way (`palai_apply_child_policy`, lines 189-192). A second copy of the project
-- on the child would be a second source of truth for one fact, and this tree refuses those on purpose.


-- The columns. DEFAULT '' NOT NULL so the ALTER rewrites existing rows to the installation-global marker
-- described above, and so this file is re-runnable -- the whole forward chain re-applies on every boot.
ALTER TABLE secret_refs  ADD COLUMN IF NOT EXISTS project_id text DEFAULT ''::text NOT NULL;
ALTER TABLE environments ADD COLUMN IF NOT EXISTS project_id text DEFAULT ''::text NOT NULL;

-- The uniqueness, and it is the half that CHANGES an answer rather than adding one. Before this file a
-- secret name was single-occupancy across the installation, so two customers could not hold `github-token`
-- separately; `environments_name_key UNIQUE (name)` said the same about an environment name, with no
-- version dimension at all.
--
-- EACH CONSTRAINT KEEPS THE BASELINE'S NAME AND CHANGES ITS DEFINITION, WHICH IS THE ONLY FORM THAT
-- SURVIVES THE NEXT BOOT. The whole forward chain re-applies every time, and 000001 re-adds these two
-- constraints under a guard that tests the NAME and nothing else:
--
--     000001_core.up.sql:2491   IF NOT EXISTS (SELECT 1 FROM pg_constraint
--                                WHERE conname = 'secret_refs_name_version_key' ...) THEN ADD CONSTRAINT ...
--
-- So a differently-named replacement leaves that guard unsatisfied, and on the SECOND boot 000001 tries to
-- build `UNIQUE (name, version)` over rows this migration has just made legal — two projects holding one
-- name. It fails with 23505 and the whole chain stops. Measured, not predicted: written that way, this file
-- took SIXTY-ONE component tests red, all with
-- `could not create unique index "secret_refs_name_version_key" (SQLSTATE 23505)`
-- (`grep -cE '^--- FAIL' postgres2.log` -> 61, 2026-08-05).
--
-- 000005 SET THE PRECEDENT and it is the same shape: it replaced `runners_capacity_check` in place rather
-- than renaming it, for this reason. The baseline is NOT edited to drop the re-add — 000001 is derived from
-- a schema dump and its checksum is journaled in schema_revisions, so rewriting it makes every existing
-- installation's recorded checksum disagree with the file that produced it.
--
-- THE COST, NAMED RATHER THAN LEFT FOR A READER TO TRIP OVER: `secret_refs_name_version_key` now covers
-- (project_id, name, version) and `environments_name_key` covers (project_id, name). The names no longer
-- describe their columns. A reader who needs the truth reads this paragraph or asks the catalogue
-- (`\d secret_refs`); a reader who trusts the name gets the pre-000006 shape. That is a genuine cost and it
-- is smaller than either alternative.
DO $$
BEGIN
    IF to_regclass('public.secret_refs') IS NOT NULL THEN
        ALTER TABLE secret_refs DROP CONSTRAINT IF EXISTS secret_refs_name_version_key;
        ALTER TABLE secret_refs
            ADD CONSTRAINT secret_refs_name_version_key UNIQUE (project_id, name, version);
    END IF;

    IF to_regclass('public.environments') IS NOT NULL THEN
        ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_name_key;
        ALTER TABLE environments
            ADD CONSTRAINT environments_name_key UNIQUE (project_id, name);
    END IF;
END
$$;

-- THE REPORT, and it runs EXACTLY ONCE -- guarded on this file's own marker the way 000005's backfill is,
-- because the chain re-applies on every boot and an unguarded report would print 104 names on every
-- restart forever.
--
-- IT IS A NOTICE AND A NOTICE CAN BE SWALLOWED, so the recovery query is written here rather than only
-- emitted: the same list is available at any time, from any psql, with
--
--     SELECT name, min(version) AS first, max(version) AS latest
--       FROM secret_refs WHERE project_id = '' GROUP BY name ORDER BY name;
--
-- A report whose only copy is a log line the operator may not have been watching is not a report.
--
-- The AMBIGUOUS half is the load-bearing one: these are the names no automation may claim, because more
-- than one project's row references them. The twelve referencing columns below are the authority
-- (information_schema, 2026-08-05: every column whose name contains secret_ref or connection_ref, all of
-- them on project-keyed tables) -- NOT a grep, which misses auth_connection_ref and connection_ref because
-- neither starts with the word.
DO $$
DECLARE
    legacy_secrets  bigint;
    legacy_envs     bigint;
    ambiguous       text;
BEGIN
    IF to_regclass('public.schema_migrations') IS NULL
       OR EXISTS (SELECT 1 FROM schema_migrations WHERE version = 6) THEN
        RETURN;
    END IF;

    SELECT count(DISTINCT name) INTO legacy_secrets FROM secret_refs  WHERE project_id = '';
    SELECT count(*)             INTO legacy_envs    FROM environments WHERE project_id = '';

    IF legacy_secrets = 0 AND legacy_envs = 0 THEN
        RAISE NOTICE '000006: no installation-wide secret or environment rows exist; every row this schema holds is project-keyed from here on.';
        RETURN;
    END IF;

    RAISE NOTICE '000006: % secret name(s) and % environment(s) stay installation-wide (project_id = ''''). They are NOT assigned to a project: nothing in this schema records who wrote them. Recover the list any time with: SELECT name FROM secret_refs WHERE project_id = '''' ORDER BY name;',
        legacy_secrets, legacy_envs;

    -- ORDER BY inside string_agg: an unordered aggregate is not deterministic, and this tree has had three
    -- outcomes decided by that omission, two of them security decisions.
    WITH refs AS (
        SELECT project_id, secret_ref          AS name FROM hooks                  WHERE coalesce(secret_ref, '')          <> ''
        UNION ALL SELECT project_id, secret_ref         FROM mcp_connections       WHERE coalesce(secret_ref, '')          <> ''
        UNION ALL SELECT project_id, secret_ref         FROM model_connections     WHERE coalesce(secret_ref, '')          <> ''
        UNION ALL SELECT project_id, secret_ref         FROM remote_tool_operations WHERE coalesce(secret_ref, '')         <> ''
        UNION ALL SELECT project_id, signing_secret_ref FROM slack_connections     WHERE coalesce(signing_secret_ref, '')  <> ''
        UNION ALL SELECT project_id, secret_ref         FROM tool_revisions        WHERE coalesce(secret_ref, '')          <> ''
        UNION ALL SELECT project_id, inbound_secret_ref      FROM triggers         WHERE coalesce(inbound_secret_ref, '')      <> ''
        UNION ALL SELECT project_id, inbound_secret_ref_next FROM triggers         WHERE coalesce(inbound_secret_ref_next, '') <> ''
        UNION ALL SELECT project_id, signing_secret_ref      FROM webhook_endpoints WHERE coalesce(signing_secret_ref, '')      <> ''
        UNION ALL SELECT project_id, signing_secret_ref_next FROM webhook_endpoints WHERE coalesce(signing_secret_ref_next, '') <> ''
        UNION ALL SELECT project_id, auth_connection_ref FROM a2a_remote_agents    WHERE coalesce(auth_connection_ref, '') <> ''
        UNION ALL SELECT project_id, connection_ref      FROM repository_bindings  WHERE coalesce(connection_ref, '')      <> ''
    )
    SELECT string_agg(name, ', ' ORDER BY name) INTO ambiguous
      FROM (SELECT name FROM refs
             WHERE name IN (SELECT name FROM secret_refs WHERE project_id = '')
             GROUP BY name HAVING count(DISTINCT project_id) > 1) a;

    IF ambiguous IS NOT NULL THEN
        RAISE NOTICE '000006: these installation-wide secret name(s) are referenced by MORE THAN ONE project and therefore cannot be claimed by any automation -- an operator must decide who owns each, and one of the projects will have to rotate to a new name: %', ambiguous;
    END IF;
END
$$;

-- The policies. Two tenant-keyed, one child -- and the tenant procedure takes TWO arguments: 000002 removed
-- the third (`has_project`) deliberately, because its body never read it and "a parameter nothing reads is
-- a boundary a reader will believe in" (000002's header, lines 19-24). A three-argument CALL fails at boot.
--
-- These are CALLs on tables 000002 already swept, so they REPLACE that file's installation policy rather
-- than adding a second one: palai_apply_tenant_policy drops `tenant_isolation` by name before creating it.
-- The order in the chain is what makes that work -- 000002 runs first on every boot, and this file runs
-- after it, every time.
CALL palai_apply_tenant_policy('secret_refs', 'project_id');
CALL palai_apply_tenant_policy('environments', 'project_id');
CALL palai_apply_child_policy('environment_values', 'environments', 'environment_id');

-- No GRANT is owed: 000001 already granted SELECT,INSERT on secret_refs and environments and
-- SELECT,INSERT,DELETE on environment_values to palai_app, and this file creates no table. The rule in
-- storage/tenant.go binds a migration that CREATES a tenant table; adding a column to an existing one
-- inherits the table's grants unchanged.

INSERT INTO schema_migrations (version) VALUES (6) ON CONFLICT DO NOTHING;
