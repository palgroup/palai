-- Reverse 000063. NOT NULL comes back on every organization_id column that can carry it; the foreign
-- keys to organizations do NOT, and that half is one-way on purpose (the 000034 precedent, for the same
-- reason spelled out below rather than asserted).
--
-- WHY THE NOT NULL HALF IS EXACT. It is derived from the same catalogue predicate the up migration used,
-- so it restores precisely the columns that migration relaxed and nothing else: before 000063 every
-- organization_id column in the chain is NOT NULL (`grep -h 'organization_id TEXT NOT NULL'
-- storage/migrations/*.up.sql | grep -vc '^--'` -> 87 declarations on 2026-08-03, 86 of them live, and no
-- migration declares a nullable one), so "every organization_id column that is nullable now" and "every
-- column 000063 relaxed" are the same set. Measured on a real Postgres (2026-08-03): 83 restored here,
-- plus the 3 primary-key members the up migration never relaxed, back to 86 NOT NULL.
--
-- WHY IT IS GUARDED BY A NULL COUNT. After Task 5 lands, rows written with no organization at all exist,
-- and SET NOT NULL on a column holding NULLs is an ERROR. MigrationDown() is one concatenated script, so
-- that error would abort the ENTIRE rollback at its first statement — every later reversal in the chain
-- would silently not run. A column with NULLs keeps them and stays nullable, and says so.
--
-- WHY THE FOREIGN KEYS ARE NOT RESTORED. The catalogue cannot say which tables had one: dropping a
-- constraint removes the only record that it existed, and 21 of the 87 organization_id columns carried a
-- foreign key while 66 did not. Re-adding one to every organization_id column would MANUFACTURE 66
-- constraints this migration never dropped — a worse reversal than none. The alternative, a table
-- recording what was dropped, buys nothing observable here: MigrationDown() is applied whole, and
-- 000001_core.down.sql drops organizations (and every table referencing it) with CASCADE at the tail of
-- that same script, so no schema state exists in which the missing constraint can be reached. `up from
-- scratch` and `down then up` still agree, which is the property the chain actually rests on. Store.
-- Rollback, the only caller of MigrationDown(), is reached from tests/component/postgres and nothing
-- else; `palai upgrade rollback` is the APPLICATION rollback (an image swap, stack/upgrade.go:197) and
-- does not run this file. An operator who needs the pre-000063 constraints back restores from the
-- pre-upgrade backup, which is the rollback-window discipline docs/operations/upgrade.md already
-- describes and the same answer 000034's reversal gives.
DO $$
DECLARE entry RECORD;
DECLARE nulls BIGINT;
DECLARE restored INT := 0;
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
           AND NOT att.attnotnull
         ORDER BY cls.relname
    LOOP
        EXECUTE format('SELECT count(*) FROM public.%I WHERE organization_id IS NULL', entry.table_name)
           INTO nulls;
        IF nulls = 0 THEN
            EXECUTE format('ALTER TABLE public.%I ALTER COLUMN organization_id SET NOT NULL', entry.table_name);
            restored := restored + 1;
        ELSE
            RAISE NOTICE '000063 down: %.organization_id holds % NULL row(s) and stays nullable',
                         entry.table_name, nulls;
        END IF;
    END LOOP;
    RAISE NOTICE '000063 down: restored NOT NULL on % organization_id column(s)', restored;
END
$$;

-- Guarded so the reversal stays idempotent even after 000001 has dropped schema_migrations (the 000029
-- down-migration precedent).
DO $$
BEGIN
    IF to_regclass('public.schema_migrations') IS NOT NULL THEN
        DELETE FROM schema_migrations WHERE version = 63;
    END IF;
END
$$;
