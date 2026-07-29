-- E24 T1: THE RUNNER REGISTRY — the table two files ordered independently and neither knew it.
--
-- runner_gateway.go:73, verbatim: "there is no hosts/runners table in this tier … that is the
-- SaaS/post-SH-0 upgrade path". local_credentials.go:122, verbatim and written without reference to
-- the first: "Bounding concurrent identities per token needs a runner registry the single-runner
-- SH-0 topology does not have; that is the upgrade path."
--
-- A CORRECTION TO THE PLAN THIS MIGRATION IMPLEMENTS, measured against the tree rather than assumed:
-- §1 calls R1 (runner_pools) and R3 (runners) NEW tenant tables. THEY ARE NOT NEW. Both — plus
-- runner_leases — have existed since 000001_core.up.sql:201-227 as empty skeletons, are already
-- listed in tests/component/postgres/migration_test.go allTables, are already named in
-- cmd/cli/internal/stack/install_backup.go:77 bootInfraTables, and — the load-bearing consequence —
-- were ALREADY swept into RLS by 000029's catalogue-driven loop and ALREADY granted by its blanket
-- GRANT, because they carried organization_id before 29 ran. So only TWO tables here are new and
-- only those two need their own GRANT. What the pre-existing pair DO need is a re-CALL of
-- palai_apply_tenant_policy, and for exactly one reason: this migration gives `runners` a project_id,
-- which changes the policy EXPRESSION 000029 derives (has_project flips false -> true). On the boot
-- that applies this file, 29 has already run against the old shape; without the re-CALL below the
-- table would carry the org-only policy until the NEXT boot.
--
-- Nothing has ever written a row to any of the three: no file under storage/queries mentions them
-- (grep, 2026-07-29) and no Go statement does either. That is what makes the runners.status ->
-- runners.state replacement below a lossless one rather than a data migration.
--
-- SIX RIDERS.

-- ---------------------------------------------------------------------------------------------------
-- R1 — runner_pools becomes a pool: a posture, a shape of machine, and a name that is unique in its
--      project. The 000001 skeleton had id/organization_id/project_id/name/created_at and nothing to
--      decide with.
-- ---------------------------------------------------------------------------------------------------

-- posture is what a pool IS: 'sandboxed-linux' is today's container runner, 'unsandboxed-host' is a
-- rented Mac. The CHECK, not an application constant, because a run is placed by this value and an
-- unrecognised posture would place it somewhere nobody described. Default matches every existing
-- deployment, which is the whole of §2's bit-unchanged rule expressed as a column default.
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS posture TEXT NOT NULL DEFAULT 'sandboxed-linux';
ALTER TABLE runner_pools DROP CONSTRAINT IF EXISTS runner_pools_posture_check;
ALTER TABLE runner_pools ADD CONSTRAINT runner_pools_posture_check
    CHECK (posture IN ('sandboxed-linux', 'unsandboxed-host'));

ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS os TEXT NOT NULL DEFAULT '';
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT '';

-- strict_enrollment is T6's waiting room, and it is DEFAULT FALSE deliberately: a machine that
-- presents a valid pool key enrols immediately, which is what every deployment does today. Flipping
-- it makes enrolment need a human. The column lands here because T6 carries no migration.
ALTER TABLE runner_pools ADD COLUMN IF NOT EXISTS strict_enrollment BOOLEAN NOT NULL DEFAULT false;

-- One pool per name per project. A UNIQUE INDEX rather than a constraint because project_id is
-- NULLABLE in the 000001 shape (an org-wide pool is representable) and the index form takes
-- IF NOT EXISTS; Postgres treats NULLs as distinct in both forms, so an org-wide pool is not
-- name-unique — which is honest, because nothing places into one yet.
CREATE UNIQUE INDEX IF NOT EXISTS runner_pools_name_key
    ON runner_pools (organization_id, project_id, name);

-- ---------------------------------------------------------------------------------------------------
-- R2 — runner_pool_keys (NEW): the credential a machine presents to enrol into ONE pool.
--      T3 writes the mint/verify; this creates the durable half so the chain stops at one file.
-- ---------------------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS runner_pool_keys (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    pool_id TEXT NOT NULL REFERENCES runner_pools (id) ON DELETE CASCADE,
    -- The HASH, never the value. The api_keys precedent (000001): the bearer is printed once at mint
    -- and the column is what a presentation is compared against, in constant time, by T3.
    key_sha256 TEXT NOT NULL,
    -- The first few characters, so an operator can tell two keys apart in a list without holding
    -- either. It is a prefix of a credential and therefore NOT a credential — the same bargain
    -- `palai apikey list` already makes.
    key_prefix TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- An expiry, because FileEnrollmentTokens today has none: the shipped bootstrap credential never
    -- stops working. NULL is "no expiry" and is what an operator has to ask for explicitly.
    expires_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    -- A key belongs to ONE pool. Global-UNIQUE rather than per-tenant because the presented value is
    -- the ONLY thing on the enrolment wire (there is no tenant on it — §3.6 D8), so the hash has to
    -- resolve to exactly one row across the whole installation or the lookup is ambiguous. The tree
    -- has decided a security outcome on a per-tenant UNIQUE plus an unordered LIMIT 1 twice already.
    UNIQUE (key_sha256)
);

CREATE INDEX IF NOT EXISTS runner_pool_keys_pool_idx ON runner_pool_keys (pool_id);

-- ---------------------------------------------------------------------------------------------------
-- R3 — runners becomes the registry. The 000001 skeleton carried id/organization_id/pool_id/status.
-- ---------------------------------------------------------------------------------------------------

-- project_id: a pool is project-scoped, so its members are. This is the column that makes the
-- palai_apply_tenant_policy re-CALL at the bottom of this file necessary rather than decorative.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS project_id TEXT REFERENCES projects (id);

-- label is what the machine called ITSELF (PALAI_RUNNER_ID). It decides nothing and is not unique:
-- runner-entrypoint.sh:10 hardcodes "runner-local", so `--scale runner=3` is three machines with one
-- label, which is precisely why it cannot be the identity.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS label TEXT NOT NULL DEFAULT '';

-- runner_dns is the SAN of the certificate the gateway issued, derived from the SERVER-minted id. It
-- is UNIQUE because it is the identity every later request actually presents — a renew carries a
-- certificate, not an id — so it is the lookup key, and two rows claiming one DNS would mean the CA
-- issued one name twice.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS runner_dns TEXT NOT NULL DEFAULT '';
CREATE UNIQUE INDEX IF NOT EXISTS runners_dns_key ON runners (runner_dns) WHERE runner_dns <> '';

ALTER TABLE runners ADD COLUMN IF NOT EXISTS public_key_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS enrolled_via_key_id TEXT REFERENCES runner_pool_keys (id);
ALTER TABLE runners ADD COLUMN IF NOT EXISTS os TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS arch TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS posture TEXT NOT NULL DEFAULT '';
ALTER TABLE runners ADD COLUMN IF NOT EXISTS capacity INTEGER NOT NULL DEFAULT 1 CHECK (capacity > 0);
ALTER TABLE runners ADD COLUMN IF NOT EXISTS cert_not_after TIMESTAMPTZ;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS enrolled_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp();
-- last_seen_at advances on enrol, connect and renew — the three moments a machine authenticates.
-- IT IS NOT A HEARTBEAT and nothing reaps on it; that is T5. An inventory, not a health source.
ALTER TABLE runners ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ;

-- status -> state. DROP + ADD rather than RENAME because the boot runner re-applies this whole chain
-- on every start and a RENAME is not idempotent, while both of these are. Dropping is LOSSLESS here
-- and that is a measured claim, not an assumption: no query file and no Go statement has ever written
-- to this table (see the header), so the column has never held a value in any deployment. The old
-- default was 'enrolled', which is not a member of the new set — a rename would have needed a data
-- fix-up for rows that do not exist.
ALTER TABLE runners DROP COLUMN IF EXISTS status;
ALTER TABLE runners ADD COLUMN IF NOT EXISTS state TEXT NOT NULL DEFAULT 'pending';
ALTER TABLE runners DROP CONSTRAINT IF EXISTS runners_state_check;
ALTER TABLE runners ADD CONSTRAINT runners_state_check
    CHECK (state IN ('pending', 'active', 'cordoned', 'revoked'));

-- The registry's two reads: everything in a pool, and the tenant-scoped keyset page the read surface
-- serves ((created_at, id) DESC — pagination.go's ordering, so the index matches the query).
CREATE INDEX IF NOT EXISTS runners_pool_idx ON runners (pool_id, state);
CREATE INDEX IF NOT EXISTS runners_tenant_page_idx
    ON runners (organization_id, project_id, created_at DESC, id DESC);

-- ---------------------------------------------------------------------------------------------------
-- R4 — runner_enrollments (NEW): the APPEND-ONLY journal. capability_jobs' shape (000040) verbatim,
--      including the load-bearing REVOKE.
-- ---------------------------------------------------------------------------------------------------

CREATE TABLE IF NOT EXISTS runner_enrollments (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations (id),
    project_id TEXT NOT NULL REFERENCES projects (id),
    -- A plain reference, not an FK, for the reason capability_jobs.worker_id is one: the journal has
    -- to survive the removal of the runner it describes. A revocation record that a DELETE can erase
    -- along with its subject is not a record.
    runner_id TEXT NOT NULL,
    pool_id TEXT NOT NULL,
    -- Which key minted this certificate. THE MISSING FACT §3.6 D5 names: revoking an enrolment key
    -- today is all-or-nothing because nothing records which key issued which identity.
    key_id TEXT NOT NULL DEFAULT '',
    entry_kind TEXT NOT NULL
        CHECK (entry_kind IN ('requested', 'approved', 'refused', 'issued', 'revoked', 'renewed')),
    entry_seq BIGINT NOT NULL CHECK (entry_seq > 0),
    -- Structured context: the label the machine claimed, the DNS issued, the refusal reason. NEVER a
    -- credential — §2 is explicit that the journal writes key_id and never a key VALUE.
    detail JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    -- The journal's spine: entries of a runner are strictly ordered and two writers computing the
    -- same next seq collide here (23505), so an append is at-most-once.
    UNIQUE (runner_id, entry_seq)
);

CREATE INDEX IF NOT EXISTS runner_enrollments_runner_seq_idx
    ON runner_enrollments (runner_id, entry_seq DESC);

-- ---------------------------------------------------------------------------------------------------
-- R5 — a run records WHERE it was placed.
-- ---------------------------------------------------------------------------------------------------

-- NULL means "no placement decision was made", which is every run in every deployment today, and it
-- is the reason this is nullable rather than defaulted to the seeded pool: a backfilled default would
-- claim a decision that was never taken. A resume reads this to return to the SAME pool (T4).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pool_id TEXT REFERENCES runner_pools (id);

-- ---------------------------------------------------------------------------------------------------
-- R6 — the boot seed, and it is the whole of "an existing single-runner install is bit-unchanged".
-- ---------------------------------------------------------------------------------------------------
--
-- A runner has to enrol INTO a pool, so an installation with no pool is an installation whose runner
-- cannot enrol. This seeds the bootstrap tenant's pool for an install UPGRADING from 000044 — it
-- already has org_local/prj_local, seeded by identity/store.go before this file ever runs a second
-- time. A FRESH stack is the other population and this statement cannot serve it: migrations run
-- BEFORE the identity bootstrap, so on first boot there is no organization to reference. That half is
-- seeded in the same transaction as the four identity rows (identity.Store.provision), which is also
-- what gives every LATER organization a default pool at the moment it is created.
--
-- The posture is today's: a sandboxed Linux container. strict_enrollment is false, so nothing starts
-- asking a human for permission on upgrade.
INSERT INTO runner_pools (id, organization_id, project_id, name, posture, strict_enrollment)
SELECT 'pool_default', p.organization_id, p.id, 'default', 'sandboxed-linux', false
  FROM projects p
 WHERE p.organization_id = 'org_local' AND p.id = 'prj_local'
    ON CONFLICT DO NOTHING;

-- ---------------------------------------------------------------------------------------------------
-- Tenancy: four tables, four policies, and the two grants that are actually missing.
-- ---------------------------------------------------------------------------------------------------
--
-- runner_pools and runners are re-CALLed because THIS FILE CHANGED THEIR SHAPE: runners gained a
-- project_id, so the expression 000029 derived on this boot (org-only) is now the wrong one. Calling
-- for runner_pools too costs one statement and means the pair are secured by the same visible rule
-- rather than by whichever of them happened not to change.
CALL palai_apply_tenant_policy('runner_pools', 'organization_id', true);
CALL palai_apply_tenant_policy('runners', 'organization_id', true);
CALL palai_apply_tenant_policy('runner_pool_keys', 'organization_id', true);
CALL palai_apply_tenant_policy('runner_enrollments', 'organization_id', true);

-- Created after 000001's and 000029's blanket `GRANT ... ON ALL TABLES`, which re-run every boot but
-- ran BEFORE this file on the boot that creates these two, so each needs its own grant or the runtime
-- role fails closed with "permission denied" rather than with the row-scoped policy. The pre-existing
-- pair need no grant: they existed when the blanket ran.
GRANT SELECT, INSERT, UPDATE, DELETE ON runner_pool_keys TO palai_app;
GRANT SELECT, INSERT ON runner_enrollments TO palai_app;

-- The load-bearing REVOKE (capability_jobs / usage_ledger / queue_effect_receipts precedent):
-- 000001's and 000029's blanket grants re-run on every boot and — now that runner_enrollments EXISTS
-- — re-hand palai_app UPDATE/DELETE on it. This file runs AFTER both (45 > 29 > 1) and no later
-- migration re-grants this table, so the REVOKE re-asserts every boot. An enrolment journal a
-- compromised runner could rewrite (to change which key issued its certificate) or delete (to erase
-- its own revocation) is not a journal.
REVOKE UPDATE, DELETE ON runner_enrollments FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (45) ON CONFLICT DO NOTHING;
