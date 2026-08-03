-- 000063 (relax organization_id, A.2 Task 4): organization_id becomes OPTIONAL, and every foreign key
-- pointing at organizations is dropped. Nothing stops writing it here — every writer still supplies a
-- real value, and no Go file changes in this migration's commit. This is the step that makes Task 5's
-- cut-over survivable: when the writers stop supplying it, the column must already tolerate the absence,
-- or the first INSERT after that change fails on a constraint this migration could have lifted earlier.
--
-- The surface, measured on 2026-08-03. `grep -v '^--'` is not decoration: the two commands appear in
-- THIS file, in comment lines, so an unfiltered re-run counts itself and reports 89/22.
--
--   grep -h 'organization_id TEXT NOT NULL' storage/migrations/*.up.sql | grep -vc '^--'   -> 87
--   grep -h 'organization_id TEXT NOT NULL REFERENCES organizations' storage/migrations/*.up.sql | grep -vc '^--'
--                                                                                          -> 21
--
-- 87 DECLARATIONS ARE 86 LIVE COLUMNS, and the missing one is not a miscount: 000001 creates
-- usage_events with an organization_id and 000034 drops that table (superseded by 000032's
-- usage_ledger), so its declaration survives in the chain's text and not in any catalogue. The loops
-- below act on the catalogue, so 86 is the number that matters here; measured against a real Postgres
-- with the chain applied through 000062 (2026-08-03): 86 organization_id columns, 86 of them NOT NULL,
-- 21 foreign keys to organizations. After this migration: 3 NOT NULL, 0 foreign keys.
--
-- Both loops below read the CATALOGUE rather than a list written here, the shape 000029 and 000062 use.
-- A hand-written list is wrong in a specific way rather than merely untidy: it is correct on the day it
-- is written and silently incomplete the day a table is added, and this chain re-runs on every boot, so
-- the omission would never announce itself.
--
-- IDEMPOTENT. A second run finds no NOT NULL left to drop and no constraint left to remove, and does
-- nothing; DROP CONSTRAINT IF EXISTS carries the rest.
--
-- WHAT THIS MIGRATION DOES NOT MAKE OPTIONAL, and it is the part Task 5 has to read. A column being
-- nullable is only ONE of the two things that can reject an absent organization. The other is
-- ROW-LEVEL SECURITY: a policy whose expression reads palai.org_id compares it to the row's own
-- organization_id, and NULL never satisfies that comparison — so the write is refused by the POLICY
-- long after the NOT NULL is gone, and an existing row whose organization_id is set to NULL becomes
-- INVISIBLE to the connection that owns it. 000062 rekeyed most tables to project_id, but not all of
-- them, and it is the remainder that still bites:
--
--   SELECT count(*) FROM pg_policies WHERE schemaname='public'
--     AND (coalesce(qual,'') LIKE '%palai.org_id%' OR coalesce(with_check,'') LIKE '%palai.org_id%');
--   -- 12, both before and after this migration (2026-08-04, chain applied through 000063)
--
--   api_keys, delivery_attempts, environment_values, environments, job_attempts, model_route_revisions,
--   organizations, principals, projects, schedule_occurrences, secret_refs, usage_ledger
--
-- Three of those are 000062's own named exclusions (api_keys, principals, usage_ledger); environments
-- and secret_refs carry no project_id column at all, so 000062's project_id-driven sweep never had them
-- as candidates; and four (delivery_attempts, job_attempts, model_route_revisions, schedule_occurrences)
-- resolve their parent's organization_id through EXISTS, so they fall when the parent's is nulled.
--
-- Measured rather than reasoned, as palai_app with both GUCs set (2026-08-04). Every probe ran TWICE —
-- once with a real organization_id, which must succeed, and once with NULL — so a refusal is the policy
-- rather than a malformed statement, and `responses` is the cross-table control because 000062 DID rekey
-- it to project_id:
--
--   responses     org=OK   null=OK                                            <- control
--   environments  org=OK   null=new row violates row-level security policy
--   secret_refs   org=OK   null=new row violates row-level security policy
--   api_keys      org=OK   null=new row violates row-level security policy
--   principals    org=OK   null=new row violates row-level security policy
--
-- And on the read side, same connection, same GUCs: a seeded environments row counted 1 before its
-- organization_id was set to NULL and 0 after.
--
-- So the exceptions to "organization_id is now optional" are TWO classes, not one: the three
-- primary-key members named at step 2, AND the tables above. Rekeying those policies is not this
-- migration's job — 000062 owns that boundary and left the exclusions deliberately — but a reader who
-- takes "nullable" for "optional" will find out from a refused INSERT, which is precisely the failure
-- step 2's own note warns about.

-- 1. Every foreign key referencing organizations. They are dropped rather than left in place because
-- Task 6 removes the organizations table itself, and a constraint pointing at a table that is going away
-- is work deferred, not work avoided. NOTE that a foreign key is not what makes the column mandatory: a
-- NULL satisfies MATCH SIMPLE. Step 2 is the half Task 5 actually depends on.
DO $$
DECLARE entry RECORD;
DECLARE dropped INT := 0;
BEGIN
    IF to_regclass('public.organizations') IS NULL THEN
        RETURN;
    END IF;
    FOR entry IN
        SELECT cls.relname AS table_name, con.conname AS constraint_name
          FROM pg_constraint con
          JOIN pg_class cls ON cls.oid = con.conrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
         WHERE con.contype = 'f'
           AND ns.nspname = 'public'
           AND con.confrelid = to_regclass('public.organizations')
         ORDER BY cls.relname, con.conname
    LOOP
        EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT IF EXISTS %I',
                       entry.table_name, entry.constraint_name);
        dropped := dropped + 1;
    END LOOP;
    RAISE NOTICE '000063: dropped % foreign key(s) referencing organizations', dropped;
END
$$;

-- 2. NOT NULL comes off every organization_id column, EXCEPT where the column is part of a primary key.
-- That is the exception to THIS LOOP; it is not the whole list of things that still require an
-- organization — see the row-level-security class in the header above, which no ALTER TABLE here can
-- lift. Postgres refuses that outright — `ALTER TABLE ... ALTER COLUMN organization_id DROP NOT NULL` on a
-- primary-key member raises `column "organization_id" is in a primary key` — so a sweep that did not skip
-- them would abort the whole chain on the first one rather than relax the other columns. Three tables are
-- in that position today, each declaring a composite primary key led by organization_id:
--
--   grep -n 'PRIMARY KEY *(.*organization_id' storage/migrations/*.up.sql   (2026-08-03)
--     000004_commands.up.sql:35            PRIMARY KEY (organization_id, project_id, id)
--     000005_config_revisions.up.sql:38    PRIMARY KEY (organization_id, project_id, id)
--     000016_delivered_messages.up.sql:38  PRIMARY KEY (organization_id, project_id, command_id)
--
-- Those three keep a mandatory organization_id after this migration, so Task 5 cannot stop supplying it
-- for commands, config_revisions or delivered_messages on the strength of this file alone. Relaxing them
-- means rebuilding a primary key, which rewrites the index every read of those tables uses; that belongs
-- with Task 6's column removal, which has to rebuild those keys anyway. The skip is announced rather than
-- silent — the NOTICE names each one — because a constraint this migration was expected to lift and did
-- not is exactly the kind of gap that is discovered by an INSERT in production. The NOTICE can only name
-- what this loop skipped, though: the row-level-security refusals in the header are invisible to it,
-- because they are not constraints on the column and nothing in this file examines them.
DO $$
DECLARE entry RECORD;
DECLARE relaxed INT := 0;
BEGIN
    FOR entry IN
        SELECT cls.relname AS table_name,
               EXISTS (SELECT 1
                         FROM pg_constraint pk
                        WHERE pk.conrelid = cls.oid
                          AND pk.contype = 'p'
                          AND att.attnum = ANY (pk.conkey)) AS in_primary_key
          FROM pg_attribute att
          JOIN pg_class cls ON cls.oid = att.attrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
         WHERE ns.nspname = 'public'
           AND cls.relkind = 'r'
           AND att.attname = 'organization_id'
           AND att.attnum > 0
           AND NOT att.attisdropped
           AND att.attnotnull
         ORDER BY cls.relname
    LOOP
        IF entry.in_primary_key THEN
            RAISE NOTICE '000063: %.organization_id stays NOT NULL — it is a primary-key member', entry.table_name;
        ELSE
            EXECUTE format('ALTER TABLE public.%I ALTER COLUMN organization_id DROP NOT NULL', entry.table_name);
            relaxed := relaxed + 1;
        END IF;
    END LOOP;
    RAISE NOTICE '000063: relaxed % organization_id column(s) to nullable', relaxed;
END
$$;

INSERT INTO schema_migrations (version) VALUES (63) ON CONFLICT DO NOTHING;
