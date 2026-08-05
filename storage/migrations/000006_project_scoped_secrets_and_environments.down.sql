-- Reverse 000006: give secret_refs and environments back the installation-wide shape 000002 left them in.
--
-- Every statement is guarded, because Store.Rollback runs the whole down chain to return a shared component
-- database to empty and may reach this file after the tables themselves are gone.
--
--
-- THE POLICIES COME OFF FIRST, AND THAT ORDER IS NOT STYLE — IT IS THE ONLY ORDER THAT WORKS. A policy is a
-- dependent object of every column its expression names, so with `tenant_isolation` still keyed on
-- project_id, dropping the column fails outright:
--
--     ERROR: cannot drop column project_id of table secret_refs because other objects depend on it
--     (SQLSTATE 2BP01)
--
-- That is measured, not anticipated: this file was written with the drops first and it took twenty-five
-- component tests red — every one of them a `Rollback()` in a fixture that has nothing to do with secrets,
-- because the component tier reverses the whole chain between tests. The same applies transitively to
-- environment_values, whose child policy names environments.project_id: its policy has to be replaced
-- before the PARENT's column can go.
--
-- palai_apply_installation_policy DROPs `tenant_isolation` by name and recreates it from an expression that
-- names no column at all, so this single call both removes the dependency and restores 000002's posture.
DO $$
BEGIN
    IF to_regclass('public.environment_values') IS NOT NULL THEN
        CALL palai_apply_installation_policy('environment_values');
    END IF;
    IF to_regclass('public.secret_refs') IS NOT NULL THEN
        CALL palai_apply_installation_policy('secret_refs');
    END IF;
    IF to_regclass('public.environments') IS NOT NULL THEN
        CALL palai_apply_installation_policy('environments');
    END IF;
EXCEPTION
    -- The procedures live in 000002 and the down chain reaches this file BEFORE 000002 drops them; a
    -- re-run, however, arrives after. An undefined procedure here means the schema is already further
    -- reversed than this statement, which is not an error to fail the rollback on.
    WHEN undefined_function THEN NULL;
END
$$;

-- Then the columns and their constraints.
--
-- THE ROWS ARE CLEARED BEFORE THE OLD CONSTRAINT IS RESTORED, and that is the second ordering that matters.
-- `UNIQUE (name, version)` is NARROWER than `UNIQUE (project_id, name, version)`: two projects may
-- legitimately hold the same name forward, and re-adding the old constraint over those rows FAILS. Dropping
-- the column first collapses any such pair, so the failure becomes a duplicate-key error on rows that
-- genuinely collide rather than a rollback that cannot complete on any database that ran the forward file.
--
-- That is honest rather than clean: this reversal LOSES the project attribution of every row written while
-- 000006 was applied, and on a database where two projects took the same name it cannot complete at all. A
-- boundary is not always reversible, and pretending otherwise would be the lie. The component tier's
-- rollback runs against databases with no such pair, which is why it is serviceable there.
DO $$
BEGIN
    IF to_regclass('public.secret_refs') IS NOT NULL THEN
        -- Same NAME in both directions, because the forward file kept the baseline's: dropping the column
        -- takes the widened constraint with it (a constraint cannot outlive a column it names), and the
        -- narrow one is rebuilt under the name 000001's guard tests for.
        ALTER TABLE secret_refs DROP CONSTRAINT IF EXISTS secret_refs_name_version_key;
        ALTER TABLE secret_refs DROP COLUMN IF EXISTS project_id;
        ALTER TABLE secret_refs
            ADD CONSTRAINT secret_refs_name_version_key UNIQUE (name, version);
    END IF;

    IF to_regclass('public.environments') IS NOT NULL THEN
        ALTER TABLE environments DROP CONSTRAINT IF EXISTS environments_name_key;
        ALTER TABLE environments DROP COLUMN IF EXISTS project_id;
        ALTER TABLE environments
            ADD CONSTRAINT environments_name_key UNIQUE (name);
    END IF;
END
$$;

-- Guarded for the reason 000004's and 000005's marker deletes are: the down chain runs 000006 before
-- 000001, but a re-run reaches this after 000001 has already dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 6;
    END IF;
END
$$;
