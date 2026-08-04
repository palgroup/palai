-- 000065 (project-scoped integrity, A.2 Task 6): every uniqueness rule, every index and every foreign key
-- that names organization_id is rebuilt WITHOUT it, keyed on project alone. Nothing stops writing the
-- column here and no Go file changes in this migration's commit — this is the step that has to land BEFORE
-- the writers stop, for a reason that has nothing to do with errors.
--
-- WHY UNIQUENESS COMES FIRST: IN A UNIQUE INDEX, NULL IS NOT EQUAL TO NULL. If a writer stopped supplying
-- organization_id while `UNIQUE (organization_id, project_id, name)` was still the rule, every row would
-- carry a NULL in the leading column and the index would admit unlimited duplicates — SILENTLY. Not an
-- error, not a warning: uniqueness would simply stop existing. Measured, with a control, against the pinned
-- image (temp table, ROLLBACK, 2026-08-04):
--
--   ('org_a','proj_a','dup')  first  -> 1 row
--   ('org_a','proj_a','dup')  again  -> ERROR: duplicate key value violates unique constraint   <- control
--   (NULL   ,'proj_a','dup')  x3     -> 3 rows, none refused
--
-- What that would have cost, concretely: two agent profiles with one name in a project, two tools with one
-- canonical_name, two slack_connections for a team — and, because `idempotency_records` and
-- `capability_jobs` are both on the list, the idempotency guarantee itself, which is a uniqueness rule
-- wearing a different word.
--
-- THE SURFACE, measured against a real Postgres with the chain applied through 000064 (2026-08-04). The
-- brief for this task carried 27, from a grep over the chain's TEXT; the catalogue says 45 indexes, and the
-- gap is not a miscount — a grep for `CREATE UNIQUE INDEX|UNIQUE (` cannot see a non-unique index at all,
-- and 15 of these are non-unique. Dropping a column drops every index that names it, so the 15 would have
-- gone silently too: not a correctness loss, a keyset-pagination and pump-scan loss with no error attached.
--
--   SELECT count(*) FROM pg_indexes WHERE schemaname='public' AND indexdef LIKE '%organization_id%';   -- 45
--   -- of them: 3 primary keys, 25 UNIQUE constraints, 2 bare unique indexes, 15 non-unique indexes.
--   SELECT count(*) FROM pg_constraint WHERE contype='f'
--     AND conkey @> ARRAY[(SELECT attnum FROM pg_attribute
--                           WHERE attrelid=conrelid AND attname='organization_id')];                  -- 66
--
-- The 66 foreign keys are the second thing neither the brief nor 000063 named. 000063 dropped the 21 keys
-- pointing AT organizations; these 66 point at projects (65) and commands (1) through a COMPOSITE key led
-- by organization_id — `FOREIGN KEY (organization_id, project_id) REFERENCES projects(organization_id, id)`
-- — so they are not reachable by looking for references to the organizations table, and they block both the
-- primary-key rebuild below and the column drop that follows in the next migration.
--
-- FIVE OF THE 66 ARE DROPPED AND NOT REPLACED, because their replacement already exists: runner_enrollments,
-- runner_pool_keys, slack_approval_deliveries, slack_message_turns and slack_reply_deliveries each already
-- carry `FOREIGN KEY (project_id) REFERENCES projects(id)` alongside the composite one. The loop below
-- decides that from the catalogue rather than from this list, so the number is a report, not an input.
--
-- ORGANIZATION_ID IS THE FIRST KEY COLUMN OF ALL 45 INDEXES (verified: the query that looks for one where
-- it is not returns zero rows), which is why removing it is a prefix removal and never reorders a key.
--
-- IDEMPOTENT, and by construction rather than by IF EXISTS: every loop below selects on "still names
-- organization_id", so a second run finds nothing to do. The rebuilt objects are renamed by removing
-- `organization_id_` from the name where it appears, which also keeps `indexdef LIKE '%organization_id%'`
-- from matching the NAME of an index whose columns no longer carry one.
--
-- ON EXISTING DATA: an installation that ever held more than one organization can have rows that are
-- distinct only by organization_id, and the narrower unique index below will refuse to build. That failure
-- is loud and is the correct outcome — it is the same collision that would otherwise be resolved by
-- whichever row happened to be written last.

-- 1. The 66 composite foreign keys come off, and their project-keyed equivalents are remembered so step 3
-- can put them back. They go FIRST because 65 of them depend on the UNIQUE (organization_id, id) on
-- projects and one depends on commands' primary key, both of which step 2 rebuilds.
--
-- A TEMP TABLE rather than a second catalogue pass: once the constraint is dropped, the catalogue no longer
-- holds what it was, so the definition has to be carried across the drop. It is session-scoped, and on a
-- re-run the loop that fills it selects nothing, so step 3 replays nothing.
--
-- NO `ON COMMIT DROP`, which is a portability fix rather than a preference: the production runner applies
-- each migration in ONE transaction (packages/coordinator/migrate.go), but the same text is also applied
-- statement-at-a-time by psql in the drills, where autocommit ends a transaction after the CREATE and the
-- table would be gone before step 1's first INSERT. Step 3 drops it explicitly instead, so both shapes end
-- with no leftover.
CREATE TEMP TABLE IF NOT EXISTS palai_65_refit_foreign_keys (
    table_name   TEXT NOT NULL,
    new_name     TEXT NOT NULL,
    local_cols   TEXT NOT NULL,
    target_table TEXT NOT NULL,
    target_cols  TEXT NOT NULL
);

DO $$
DECLARE entry RECORD;
DECLARE dropped INT := 0;
DECLARE kept INT := 0;
BEGIN
    FOR entry IN
        SELECT con.conname                                 AS constraint_name,
               cls.relname                                 AS table_name,
               tgt.relname                                 AS target_table,
               replace(con.conname, 'organization_id_', '') AS new_name,
               (SELECT string_agg(quote_ident(att.attname), ', ' ORDER BY ord.pos)
                  FROM unnest(con.conkey) WITH ORDINALITY AS ord(attnum, pos)
                  JOIN pg_attribute att ON att.attrelid = con.conrelid AND att.attnum = ord.attnum
                 WHERE att.attname <> 'organization_id')   AS local_cols,
               (SELECT string_agg(quote_ident(att.attname), ', ' ORDER BY ord.pos)
                  FROM unnest(con.confkey) WITH ORDINALITY AS ord(attnum, pos)
                  JOIN pg_attribute att ON att.attrelid = con.confrelid AND att.attnum = ord.attnum
                 WHERE att.attname <> 'organization_id')   AS target_cols
          FROM pg_constraint con
          JOIN pg_class cls ON cls.oid = con.conrelid
          JOIN pg_class tgt ON tgt.oid = con.confrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
         WHERE con.contype = 'f'
           AND ns.nspname = 'public'
           AND EXISTS (SELECT 1 FROM pg_attribute att
                        WHERE att.attrelid = con.conrelid
                          AND att.attname = 'organization_id'
                          AND att.attnum = ANY (con.conkey))
         ORDER BY cls.relname, con.conname
    LOOP
        EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT %I', entry.table_name, entry.constraint_name);
        dropped := dropped + 1;
        -- A constraint already enforcing the same reference is the replacement; adding a second one would
        -- double the check on every write for no extra guarantee. Judged on the columns, not the name.
        IF EXISTS (
            SELECT 1
              FROM pg_constraint c2
              JOIN pg_class t2 ON t2.oid = c2.confrelid
             WHERE c2.conrelid = to_regclass('public.' || quote_ident(entry.table_name))
               AND c2.contype = 'f'
               AND t2.relname = entry.target_table
               AND (SELECT string_agg(quote_ident(att.attname), ', ' ORDER BY ord.pos)
                      FROM unnest(c2.conkey) WITH ORDINALITY AS ord(attnum, pos)
                      JOIN pg_attribute att ON att.attrelid = c2.conrelid AND att.attnum = ord.attnum)
                   = entry.local_cols)
        THEN
            RAISE NOTICE '000065: %.% dropped; an equivalent (%) already exists',
                         entry.table_name, entry.constraint_name, entry.local_cols;
            kept := kept + 1;
        ELSE
            INSERT INTO palai_65_refit_foreign_keys
                 VALUES (entry.table_name, entry.new_name, entry.local_cols, entry.target_table, entry.target_cols);
        END IF;
    END LOOP;
    RAISE NOTICE '000065: dropped % composite foreign key(s); % had a project-keyed equivalent already',
                 dropped, kept;
END
$$;

-- 2. Every index and constraint whose key columns include organization_id is rebuilt without it.
--
-- The replacement is assembled from the CATALOGUE, not from text surgery on pg_get_indexdef's output: each
-- key column is read back with the per-column form pg_get_indexdef(oid, n, true), which carries its own
-- DESC/NULLS/opclass, INCLUDE columns come from the attributes past indnkeyatts, and a partial index's
-- predicate from pg_get_expr(indpred). Five of these are partial and one carries an INCLUDE list; a
-- substring edit that dropped either would produce an index that still exists, still gets used, and no
-- longer means what it did.
--
-- A UNIQUE constraint is rebuilt as a UNIQUE constraint and a primary key as a primary key, rather than all
-- three collapsing into bare unique indexes: a foreign key can reference the first two and this chain has
-- 66 of those to put back in step 3.
DO $$
DECLARE entry RECORD;
DECLARE key_cols TEXT;
DECLARE include_cols TEXT;
DECLARE rebuilt INT := 0;
DECLARE redundant INT := 0;
BEGIN
    FOR entry IN
        SELECT ic.relname                                  AS index_name,
               cls.relname                                 AS table_name,
               replace(ic.relname, 'organization_id_', '') AS new_name,
               con.contype                                 AS constraint_type,
               con.conname                                 AS constraint_name,
               idx.indisunique                             AS is_unique,
               idx.indnkeyatts                             AS key_count,
               idx.indnatts                                AS total_count,
               idx.indexrelid                              AS index_oid,
               idx.indrelid                                AS table_oid,
               pg_get_expr(idx.indpred, idx.indrelid)      AS predicate,
               -- The organization-free key columns, as the catalogue renders each one, WITH ITS SORT
               -- DIRECTION. pg_get_indexdef's per-column form renders the column and its opclass and
               -- STOPS THERE: it does not carry DESC or NULLS FIRST/LAST, which live in pg_index.indoption
               -- (bit 0 = descending, bit 1 = nulls first). Five of the indexes rebuilt here are keyset or
               -- newest-first indexes whose whole purpose is the DESC — a2a_remote_agents_project_idx,
               -- runners_tenant_page_idx, sessions_tenant_keyset_idx, usage_ledger_tenant_keyset_idx,
               -- usage_ledger_tenant_meter_idx — and the first cut of this migration dropped it from all
               -- five. It raised nothing: the indexes existed, were valid, were used, and scanned backwards.
               -- Both flags are emitted explicitly rather than only when they deviate from the default,
               -- because the default for NULLS depends on the direction (ASC => LAST, DESC => FIRST) and a
               -- rule with two cases is a rule with a wrong case.
               (SELECT string_agg(
                         pg_get_indexdef(idx.indexrelid, n, true)
                         || CASE WHEN (idx.indoption[n - 1] & 1) = 1 THEN ' DESC' ELSE ' ASC' END
                         || CASE WHEN (idx.indoption[n - 1] & 2) = 2 THEN ' NULLS FIRST' ELSE ' NULLS LAST' END,
                         ', ' ORDER BY n)
                  FROM generate_series(1, idx.indnkeyatts) n
                 WHERE (SELECT att.attname FROM pg_attribute att
                         WHERE att.attrelid = idx.indrelid AND att.attnum = idx.indkey[n - 1])
                       IS DISTINCT FROM 'organization_id') AS new_key_cols,
               (SELECT string_agg(pg_get_indexdef(idx.indexrelid, n, true), ', ' ORDER BY n)
                  FROM generate_series(idx.indnkeyatts + 1, idx.indnatts) n)  AS new_include_cols,
               -- The plain column names, for the forms (UNIQUE/PRIMARY KEY constraints) that take no
               -- ordering or opclass at all.
               (SELECT string_agg(quote_ident(att.attname), ', ' ORDER BY n)
                  FROM generate_series(1, idx.indnkeyatts) n
                  JOIN pg_attribute att ON att.attrelid = idx.indrelid AND att.attnum = idx.indkey[n - 1]
                 WHERE att.attname <> 'organization_id') AS new_plain_cols,
               -- A rebuilt index whose remaining columns are exactly the table's primary key would be a
               -- second copy of that key. projects' UNIQUE (organization_id, id) is the one such case.
               ((SELECT array_agg(k ORDER BY k)
                   FROM unnest(idx.indkey::smallint[]) k
                  WHERE k <> (SELECT att.attnum FROM pg_attribute att
                               WHERE att.attrelid = idx.indrelid AND att.attname = 'organization_id'))
                = (SELECT array_agg(k ORDER BY k) FROM pg_constraint pk, unnest(pk.conkey) k
                    WHERE pk.conrelid = idx.indrelid AND pk.contype = 'p')
                AND NOT idx.indisprimary) AS duplicates_primary_key
          FROM pg_index idx
          JOIN pg_class ic ON ic.oid = idx.indexrelid
          JOIN pg_class cls ON cls.oid = idx.indrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
          LEFT JOIN pg_constraint con ON con.conindid = idx.indexrelid AND con.contype IN ('p', 'u')
         WHERE ns.nspname = 'public'
           AND EXISTS (SELECT 1 FROM pg_attribute att
                        WHERE att.attrelid = idx.indrelid
                          AND att.attname = 'organization_id'
                          AND att.attnum = ANY (idx.indkey::smallint[]))
         ORDER BY cls.relname, ic.relname
    LOOP
        IF entry.constraint_type IS NOT NULL THEN
            EXECUTE format('ALTER TABLE public.%I DROP CONSTRAINT %I', entry.table_name, entry.constraint_name);
        ELSE
            EXECUTE format('DROP INDEX public.%I', entry.index_name);
        END IF;

        IF entry.duplicates_primary_key THEN
            RAISE NOTICE '000065: %.% dropped, not rebuilt — its remaining columns (%) ARE the primary key',
                         entry.table_name, entry.index_name, entry.new_plain_cols;
            redundant := redundant + 1;
            CONTINUE;
        END IF;

        IF entry.constraint_type = 'p' THEN
            EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT %I PRIMARY KEY (%s)',
                           entry.table_name, entry.new_name, entry.new_plain_cols);
        ELSIF entry.constraint_type = 'u' THEN
            EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT %I UNIQUE (%s)',
                           entry.table_name, entry.new_name, entry.new_plain_cols);
        ELSE
            key_cols := entry.new_key_cols;
            include_cols := CASE WHEN entry.new_include_cols IS NULL THEN ''
                                 ELSE ' INCLUDE (' || entry.new_include_cols || ')' END;
            EXECUTE format('CREATE %s INDEX %I ON public.%I (%s)%s%s',
                           CASE WHEN entry.is_unique THEN 'UNIQUE' ELSE '' END,
                           entry.new_name, entry.table_name, key_cols, include_cols,
                           CASE WHEN entry.predicate IS NULL THEN '' ELSE ' WHERE ' || entry.predicate END);
        END IF;
        rebuilt := rebuilt + 1;
    END LOOP;
    RAISE NOTICE '000065: rebuilt % index(es)/constraint(s) without organization_id; % were redundant and dropped',
                 rebuilt, redundant;
END
$$;

-- 3. The foreign keys go back, project-keyed, now that the keys they reference exist in that shape.
DO $$
DECLARE entry RECORD;
DECLARE restored INT := 0;
BEGIN
    FOR entry IN SELECT * FROM palai_65_refit_foreign_keys ORDER BY table_name, new_name LOOP
        EXECUTE format('ALTER TABLE public.%I ADD CONSTRAINT %I FOREIGN KEY (%s) REFERENCES public.%I (%s)',
                       entry.table_name, entry.new_name, entry.local_cols, entry.target_table, entry.target_cols);
        restored := restored + 1;
    END LOOP;
    RAISE NOTICE '000065: restored % project-keyed foreign key(s)', restored;
END
$$;

DROP TABLE IF EXISTS palai_65_refit_foreign_keys;

-- 4. The three columns 000063 could not relax. They were NOT NULL because they were primary-key members,
-- and Postgres refuses DROP NOT NULL on one — step 2 has just rebuilt those three keys without them, so the
-- last mandatory organization_id in the schema comes off here. 000063's own note said this belonged with
-- the key rebuild; this is it.
DO $$
DECLARE entry RECORD;
DECLARE relaxed INT := 0;
BEGIN
    FOR entry IN
        SELECT cls.relname AS table_name
          FROM pg_attribute att
          JOIN pg_class cls ON cls.oid = att.attrelid
          JOIN pg_namespace ns ON ns.oid = cls.relnamespace
         WHERE ns.nspname = 'public' AND cls.relkind = 'r'
           AND att.attname = 'organization_id' AND att.attnum > 0
           AND NOT att.attisdropped AND att.attnotnull
         ORDER BY cls.relname
    LOOP
        EXECUTE format('ALTER TABLE public.%I ALTER COLUMN organization_id DROP NOT NULL', entry.table_name);
        relaxed := relaxed + 1;
    END LOOP;
    RAISE NOTICE '000065: relaxed the last % mandatory organization_id column(s)', relaxed;
END
$$;

-- 5. The migration proves its own postcondition rather than asserting it in prose. A rebuild loop that
-- skipped an object would otherwise leave a schema that looks migrated and is not, and the next migration —
-- which drops the column — would be the one that discovered it.
DO $$
DECLARE leftover_indexes INT;
DECLARE leftover_keys INT;
DECLARE leftover_notnull INT;
BEGIN
    SELECT count(*) INTO leftover_indexes
      FROM pg_indexes WHERE schemaname = 'public' AND indexdef LIKE '%organization_id%';
    SELECT count(*) INTO leftover_keys
      FROM pg_constraint con
     WHERE con.contype = 'f'
       AND EXISTS (SELECT 1 FROM pg_attribute att
                    WHERE att.attrelid = con.conrelid AND att.attname = 'organization_id'
                      AND att.attnum = ANY (con.conkey));
    SELECT count(*) INTO leftover_notnull
      FROM pg_attribute att
      JOIN pg_class cls ON cls.oid = att.attrelid
      JOIN pg_namespace ns ON ns.oid = cls.relnamespace
     WHERE ns.nspname = 'public' AND cls.relkind = 'r' AND att.attname = 'organization_id'
       AND att.attnum > 0 AND NOT att.attisdropped AND att.attnotnull;
    IF leftover_indexes <> 0 OR leftover_keys <> 0 OR leftover_notnull <> 0 THEN
        RAISE EXCEPTION '000065 did not finish: % index(es), % foreign key(s), % NOT NULL column(s) still name organization_id',
                        leftover_indexes, leftover_keys, leftover_notnull;
    END IF;
END
$$;

INSERT INTO schema_migrations (version) VALUES (65) ON CONFLICT DO NOTHING;
