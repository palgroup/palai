-- 000067 (drop organization_id and the organizations table, A.2 Task 6's last step): the column comes off
-- every table that still carries it, and then the table itself goes. Nothing in Go has read either since
-- Task 5 stopped writing them and Task 6's sweep removed the last test that named them; what is left here
-- is the schema still SAYING a boundary exists.
--
-- WHY THE COLUMN GOES RATHER THAN JUST THE TABLE, and it is the whole point of this file. Since 000063 the
-- column is nullable and since 000065 no index, foreign key or NOT NULL names it, so it is already inert
-- — but an inert column that every SELECT * and every schema dump still shows reads to the next person as
-- a tenant key that someone forgot to fill in. That is the same failure this tree records in comments and
-- in dead symbols: a shape that is declared but not wired. Measured against the live chain before this
-- migration (2026-08-04, chain applied through 000066):
--
--   SELECT count(*) FROM information_schema.columns
--    WHERE table_schema='public' AND column_name='organization_id';                     -- 86
--   SELECT count(DISTINCT table_name) FROM information_schema.columns
--    WHERE table_schema='public' AND column_name='organization_id';                     -- 86
--   SELECT count(*) FROM pg_index i JOIN pg_class c ON c.oid=i.indexrelid
--     JOIN pg_attribute a ON a.attrelid=i.indrelid AND a.attnum=ANY(i.indkey)
--    WHERE a.attname='organization_id';                                                 -- 0  (000065)
--   SELECT count(*) FROM pg_constraint con JOIN pg_attribute att
--     ON att.attrelid=con.conrelid AND att.attnum=ANY(con.conkey)
--    WHERE con.contype='f' AND att.attname='organization_id';                           -- 0  (000063)
--   SELECT count(*) FROM information_schema.views
--    WHERE table_schema='public' AND view_definition LIKE '%organization_id%';          -- 0
--
-- 86 columns across 86 tables, and nothing depends on any of them. The zeroes matter because DROP COLUMN
-- CASCADEs into indexes and constraints silently: with them at zero, this migration cannot take a
-- constraint down as a side effect that nobody wrote down.
--
-- ONE READER SURVIVES THE DROP AND IT IS NOT SQL: packages/audit.ChainedColumns lists `organization_id`
-- as a column the SEC-103 row digest covers, and a component guard asserts that list equals the `events`
-- table's ACTUAL column set. The Go side of this commit removes it from both the constant and the digest.
-- THAT CHANGES EVERY ROW DIGEST, so checkpoints cut before this migration do not verify against rows read
-- after it — which is the behaviour packages/audit already documents for a schema change ("re-cut a
-- checkpoint after such an upgrade"). No committed evidence bundle pins a chain digest, so the cost is
-- paid by re-cutting, not by a released artefact going stale.
--
-- The AuditChainRows query had ALREADY stopped selecting the column, while ChainedColumns still named it.
-- That gap was invisible to the guard, which compares the constant to the TABLE and not to the query: a
-- column inside the constant but outside the SELECT is a column the digest claims to cover and does not.
-- Dropping the column closes it from both ends.
--
-- IDEMPOTENT. A second run finds no column named organization_id and no organizations table, and does
-- nothing. Both loops read the CATALOGUE rather than a list written here — the shape 000063 and 000065
-- use — because a hand-written list is correct on the day it is written and silently incomplete the day a
-- table is added, and this chain re-runs on every boot.

-- 1. The column, off every table that still has one. ORDER BY makes the NOTICE stable across runs, which
-- matters only because an operator reading two boot logs should be able to diff them.
DO $$
DECLARE entry RECORD;
DECLARE dropped INT := 0;
BEGIN
    FOR entry IN
        SELECT cls.relname AS table_name
          FROM pg_attribute att
          JOIN pg_class cls ON cls.oid = att.attrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
         WHERE ns.nspname = 'public'
           AND cls.relkind = 'r'
           AND att.attname = 'organization_id'
           AND att.attnum > 0
           AND NOT att.attisdropped
         ORDER BY cls.relname
    LOOP
        EXECUTE format('ALTER TABLE public.%I DROP COLUMN IF EXISTS organization_id', entry.table_name);
        dropped := dropped + 1;
    END LOOP;
    RAISE NOTICE '000067: dropped organization_id from % table(s)', dropped;
END
$$;

-- 2. The table. Its own row-level-security policy (`id = palai.org_id`) goes with it, and that policy is
-- the ONE 000066 deliberately left standing — 000066's postcondition names `organizations` as its single
-- expected survivor, so this is the step that makes the count zero rather than one.
--
-- NO CASCADE, and the restraint is the assertion: every foreign key pointing at this table was dropped by
-- 000063, so a plain DROP must succeed. If one has come back, this migration fails loudly here instead of
-- quietly taking someone else's constraint with it.
DROP TABLE IF EXISTS organizations;

-- 3. The postcondition, asserted rather than described — all three of the things a reader would otherwise
-- have to take on trust, in the same statement so a partial result cannot read as a whole one.
DO $$
DECLARE leftover_columns INT;
DECLARE leftover_policies TEXT;
BEGIN
    SELECT count(*) INTO leftover_columns
      FROM pg_attribute att
      JOIN pg_class cls ON cls.oid = att.attrelid
      JOIN pg_namespace ns ON ns.oid = cls.relnamespace
     WHERE ns.nspname = 'public' AND cls.relkind = 'r' AND att.attname = 'organization_id'
       AND att.attnum > 0 AND NOT att.attisdropped;

    SELECT string_agg(tablename, ', ' ORDER BY tablename) INTO leftover_policies
      FROM pg_policies
     WHERE schemaname = 'public'
       AND (coalesce(qual, '') LIKE '%palai.org_id%' OR coalesce(with_check, '') LIKE '%palai.org_id%');

    IF leftover_columns <> 0 THEN
        RAISE EXCEPTION '000067 did not finish: % column(s) are still named organization_id', leftover_columns;
    END IF;
    IF to_regclass('public.organizations') IS NOT NULL THEN
        RAISE EXCEPTION '000067 did not finish: the organizations table is still present';
    END IF;
    IF leftover_policies IS NOT NULL THEN
        RAISE EXCEPTION '000067 did not finish: these policies still read palai.org_id: %', leftover_policies;
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (67) ON CONFLICT DO NOTHING;
