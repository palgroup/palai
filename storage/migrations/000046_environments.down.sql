-- Reverse 000046. Every statement is guarded on its target still existing, because MigrationDown() is
-- one concatenated chain and re-running it (or running it after 000019 has already dropped
-- agent_revisions) must stay a clean no-op — 000045's and 000044's reversals are written the same way.
--
-- THE HONEST SHAPE OF THIS ROLLBACK, AND IT IS ASYMMETRIC ON PURPOSE. The two tables 000046 created
-- are dropped, so every environment and every key BINDING is gone. THE VALUES ARE NOT: they are
-- secret_refs rows under the derived name `env:<environment_id>:<key>`, 000031 owns that table, and
-- this file has no grant to delete from it even if it wanted one. So a rollback across this line
-- leaves the sealed bytes in place with nothing naming them — orphaned, unreachable, and retained
-- exactly the way every other superseded secret version is retained.
--
-- That is worth reading twice before someone treats a rollback as an erasure: rolling this back
-- removes an operator's ability to SEE which keys an environment had. It removes no credential. A
-- deployment that needs the bytes gone needs a secret_refs retention sweep, which does not exist
-- (000031:45 names it as the future that would re-grant DELETE narrowly).

-- environment_values first: it references environments, and while ON DELETE CASCADE would handle the
-- rows, dropping the parent first would fail on the constraint itself.
DROP TABLE IF EXISTS environment_values;
DROP TABLE IF EXISTS environments;

-- The bond. Guarded on the table because 000019 owns agent_revisions and may already have dropped it.
DO $$
BEGIN
    IF to_regclass('public.agent_revisions') IS NOT NULL THEN
        ALTER TABLE agent_revisions DROP COLUMN IF EXISTS environment;
    END IF;
END
$$;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations.
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 46;
    END IF;
END
$$;
