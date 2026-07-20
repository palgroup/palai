-- Repository binding + deterministic-preparation receipt (spec §30.1, §30.3). A
-- RepositoryBinding is the durable, policy-carrying record that connects a project to one
-- external repository: provider + IMMUTABLE provider repository identity (display names and
-- clone URLs are NOT trusted as identity, §30.1 — the installation/repository id is
-- authoritative), the clone/fetch URL, the default branch, the auth Connection reference the
-- credential broker mints short-lived tokens against (never the raw key), the allowed
-- read/write/publication operations, and the branch/path/submodule/LFS/commit-signing/
-- fork-PR-target policy bundle.
--
-- Preparation is INFRASTRUCTURE-owned (spec §30.3): the 11-step deterministic clone RECORDS a
-- receipt — base commit, tree hash, branch — whose provenance does NOT depend on model behavior
-- (§30.3 line 3273, REP-001). A later run reads it to prove the exact tree the engine was handed,
-- before any model ran a Git command. Re-preparation (a new attempt) appends a new receipt row;
-- the provenance ledger is append-only.
--
-- CREATE ... IF NOT EXISTS keeps the migration idempotent (Migrate is re-run per boot), matching
-- the 000007/000008 pattern. Tenant scope is the composite (organization_id, project_id) FK to
-- projects every execution row carries (spec §39.2); the new tables are granted to the palai_app
-- role explicitly (000001's blanket GRANT covered only the tables that existed then).

CREATE TABLE IF NOT EXISTS repository_bindings (
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- Provider + the IMMUTABLE provider repository identity. Display names and clone URLs are not
    -- trusted as identity (spec §30.1); the provider installation/repository id is authoritative.
    provider TEXT NOT NULL,
    repository_identity TEXT NOT NULL,
    clone_url TEXT NOT NULL,
    default_branch TEXT NOT NULL DEFAULT 'main',
    -- The auth Connection reference the broker mints repository-scoped tokens against (spec §30.2).
    -- The raw credential / GitHub App private key is NEVER stored here — only the reference; the
    -- root key stays behind the LP-0 file-secret bridge until E13 seals it (§7 deferral).
    connection_ref TEXT NOT NULL DEFAULT '',
    -- Allowed read/write/publication operations (spec §30.1): a JSONB set the policy evaluator reads.
    allowed_operations JSONB NOT NULL DEFAULT '[]',
    -- The branch/path/submodule/LFS/commit-signing/fork-PR-target policy bundle (spec §30.1). One
    -- JSONB document rather than six columns: it is a nested policy doc the preparation and
    -- publication paths read as a whole, not a set of independently-queried scalars.
    policy JSONB NOT NULL DEFAULT '{}',
    data_classification TEXT NOT NULL DEFAULT '',
    region_constraint TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE IF NOT EXISTS preparation_receipts (
    id TEXT PRIMARY KEY,
    repository_binding_id TEXT NOT NULL REFERENCES repository_bindings (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The run whose workspace this preparation filled (nullable like workspaces.run_id: a
    -- session-only or test preparation carries none). A new attempt appends a new receipt row.
    run_id TEXT REFERENCES runs (id),
    -- The requested ref (branch/tag/sha) and the exact commit it RESOLVED to. base_commit +
    -- tree_hash are the MODEL-INDEPENDENT provenance (spec §30.3 step 10, REP-001): the exact tree
    -- the engine was handed, recorded by infrastructure before any model behavior.
    requested_ref TEXT NOT NULL DEFAULT '',
    base_commit TEXT NOT NULL,
    tree_hash TEXT NOT NULL,
    -- The generated work branch checked out, or empty for the detached read-only state (§30.3 step 6).
    branch TEXT NOT NULL DEFAULT '',
    prepared_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The new tables are DML-granted to the application role (000001's GRANT ON ALL TABLES only
-- covered tables that existed then; a security boundary, so it is explicit here, not inherited).
GRANT SELECT, INSERT, UPDATE, DELETE ON repository_bindings, preparation_receipts TO palai_app;

INSERT INTO schema_migrations (version) VALUES (9) ON CONFLICT DO NOTHING;
