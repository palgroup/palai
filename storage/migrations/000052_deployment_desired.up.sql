-- 000052 (E29 desired configuration): the DESIRED configuration of this MACHINE, written by the admin
-- panel and applied by the next bring-up.
--
-- WHAT WAS MISSING, and it is not "a table". GET /v1/deployment (941a1d7c) made thirty-five settings
-- readable and every one of them read-only, for a measured reason — on the running stack, 2026-08-01:
--
--   curl -s .../v1/deployment | jq -r '.settings[].mutability' | sort | uniq -c
--     32 bring_up
--      3 bring_up_default_only
--
-- Thirty-two are read from the PROCESS ENVIRONMENT, which is fixed at exec, so there is no live write
-- path for them and inventing one would be the "declared, and nothing happens" defect this tree keeps
-- finding. What was missing is the OTHER half: somewhere durable and server-side for an operator to say
-- what the machine SHOULD run with, so the value lives on the machine rather than in a dotenv file on
-- somebody's laptop, and so the next bring-up can apply it.
--
-- IT IS NOT config_policy, AND THE SECOND REASON IS THE ONE THAT DECIDES IT.
--
--   The first reason is scope-of-meaning: config_policy is a JSONB column on the PROJECTS table
--   (000005:16), written per project through PATCH /v1/projects/{id}. A dispatch worker count is a
--   property of the process every project on this machine shares, and a machine-wide switch behind a
--   project picker would look per-project to the one reader who most needs to know it is not.
--
--   The second is authority. config_policy is TENANT-scoped: an org-admin writes their own project's
--   document. Four of the eleven writable settings here are ADMISSION BOUNDS — PALAI_MAX_CONCURRENT_RUNS,
--   PALAI_MAX_QUEUED_RUNS, PALAI_REQUEST_RATE_PER_SEC, PALAI_REQUEST_BURST — which exist to bound a
--   tenant. Storing them anywhere a tenant can reach would let a tenant raise the limit that holds it.
--   So this table carries NO organization_id and NO project_id, and that absence is the security
--   property: there is no column a per-tenant policy could key on, so the surface cannot become
--   per-tenant by a later edit that adds one predicate. It is reached under storage.WithSystemScope and
--   gated on the `provision` capability, the same authority the tenancy surface requires.
--
--   Being non-tenant, it takes no palai_apply_tenant_policy call (000029's catalogue loop only touches
--   tables carrying organization_id) and it goes on tests/security/tenancy's nonTenantTables allow-list
--   BY NAME — which is the point of that list: a table that lands there without a decision fails
--   TestEveryTenantTableIsRowLevelSecured instead of silently leaking, and a table that lands there WITH
--   one is a sentence somebody wrote.
--
-- APPEND-ONLY, ONE ROW PER REVISION, holding the WHOLE document.
--
--   A row-per-setting table would need an UPDATE path and a DELETE path, and "remove this key" would be
--   a different operation from "change this key" — where the semantics are the same: what the next
--   bring-up exports. A whole-document write makes removal expressible as absence, which is what the API
--   needs (a key that is present means the operator decided; a key that is absent means the deployment's
--   own default), and it makes the write ONE statement with no read-modify-write race in it.
--
--   It is APPEND-ONLY because "we must be able to see it" is half the requirement and a mutable row
--   answers only the present tense. An operator asking why this machine is running four workers wants
--   the revision, the time and the principal — and an UPDATE would have erased the answer. The REVOKE
--   below is what makes that structural rather than a convention, and it is SELF-RE-ASSERTING for the
--   reason 000032/000033/000045 all record: 000001's and 000029's blanket `GRANT ... ON ALL TABLES` re-run
--   on every boot and would re-hand palai_app UPDATE/DELETE on this table once it exists. 52 > 29 > 1 in
--   the chain and no later migration re-grants it, so this REVOKE re-asserts every boot.
CREATE TABLE IF NOT EXISTS deployment_desired (
    -- The revision is the ORDERING and the identity. GENERATED ALWAYS AS IDENTITY rather than a
    -- timestamp: two writes inside one clock tick are two revisions, and a reader taking "the latest"
    -- must never have to break a tie. Every read below orders by it explicitly — this tree has had an
    -- unordered LIMIT decide a security outcome twice.
    revision BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    -- The whole desired document: {"PALAI_DISPATCH_WORKERS":"4", …}. JSONB rather than TEXT so the
    -- shape is the database's business too, and an object rather than an array so a key cannot appear
    -- twice with two values.
    --
    -- NO CHECK CONSTRAINT NAMING THE WRITABLE SETTINGS, deliberately. The allow-list is computed from
    -- the catalogue in Go (api.desiredWritable, which drops every path-kind setting structurally), and
    -- a half-expressed copy of it here would be read as the whole rule the day the two disagree. What
    -- the database asserts is the shape it can actually enforce.
    document JSONB NOT NULL,
    CONSTRAINT deployment_desired_document_is_object CHECK (jsonb_typeof(document) = 'object'),
    written_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- The principal id from the VERIFIED credential, never a body field. It is an id and not a name:
    -- this table is read back onto an operator screen and a display name would be a second copy of
    -- something identity already owns.
    written_by TEXT NOT NULL
);

GRANT SELECT, INSERT ON deployment_desired TO palai_app;
REVOKE UPDATE, DELETE ON deployment_desired FROM palai_app;

INSERT INTO schema_migrations (version) VALUES (52) ON CONFLICT DO NOTHING;
