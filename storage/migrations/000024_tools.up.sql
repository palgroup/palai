-- Extensibility registry (spec §28.2-28.4, E12 Task 2). A `tools` row is a durable named lineage; a
-- `tool_revisions` row is an IMMUTABLE, publishable snapshot of one executor binding (control_plane |
-- remote_http | mcp), digest-addressed; a `tool_set_revisions` row is a named, publishable, exact-pin
-- set of published tool revisions an AgentRevision references. Built-in tools (file/shell/commit/push/
-- pull_request/math) are NOT seeded here — they stay code-defined (ponytail); the registry carries only
-- remote/MCP executor references (T2 ships the control_plane echo binder as the load-into-broker proof).
--
-- Immutability is by DISCIPLINE, mirroring 000019 agents: publish is the ONE legitimate mutation (a
-- conditional published_at flip), so UPDATE stays granted; no statement ever rewrites a revision's config
-- columns — a "revise" INSERTs a NEW revision. Every CREATE/ADD COLUMN IF NOT EXISTS keeps the migration
-- idempotent (Migrate is re-run per boot). Tenant scope is the composite (organization_id, project_id) FK.

CREATE TABLE IF NOT EXISTS tools (
    id TEXT PRIMARY KEY,                       -- tool_<hex>
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The publisher.namespace.tool canonical name; case-sensitive ASCII, app-validated format + length.
    -- Unique per project — a second lineage with the same canonical name is rejected.
    canonical_name TEXT NOT NULL,
    -- The deterministic model-visible short name (canonical's last segment). Unique per project so two
    -- tools never expose the SAME name to the model: collision is a create REJECT, never an auto-suffix.
    model_visible_name TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, canonical_name),
    UNIQUE (organization_id, project_id, model_visible_name)
);

CREATE TABLE IF NOT EXISTS tool_revisions (
    id TEXT PRIMARY KEY,                       -- trev_<hex>
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    tool_id TEXT NOT NULL REFERENCES tools (id),
    -- Monotonic per tool: a revise is a new revision_number, never an in-place edit.
    revision_number INTEGER NOT NULL,
    -- The executor kind (control_plane | remote_http | mcp). App-validated; NO CHECK — T4/T5 add kinds
    -- without a migration. In T2 only control_plane has a binder (the pure echo handler); a remote_http /
    -- mcp row is creatable but binder-less, so it is never resolved or advertised (no dead advertisement).
    executor TEXT NOT NULL,
    -- The tenant-written description is UNTRUSTED text; publish is the admin approval point (spec §28.4).
    description TEXT NOT NULL DEFAULT '',
    input_schema JSONB NOT NULL,
    output_schema JSONB,
    replay_class TEXT NOT NULL DEFAULT 'pure',
    -- Declared limits; enforcement lives on the executors (T4/T5), not here.
    timeout_ms INTEGER,
    limits JSONB,
    -- executor_config carries only NON-secret wiring; a credential is a secret_ref HANDLE, never inline
    -- bytes (spec §28.4 secret hygiene) — the raw credential never enters a registry row.
    executor_config JSONB,
    secret_ref TEXT,
    -- sha256 over the revision's canonical config (the config.go content-hash pattern), so an identical
    -- revision addresses identically and a published row is verifiably frozen.
    digest TEXT NOT NULL,
    -- NULL = draft (cannot be pinned); a conditional flip publishes it exactly once (irreversible).
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (tool_id, revision_number)
);

-- Read a tool's revisions newest-first (management lists them; the broker lookup reads a pinned one).
CREATE INDEX IF NOT EXISTS tool_revisions_by_tool ON tool_revisions (tool_id, revision_number DESC);

CREATE TABLE IF NOT EXISTS tool_set_revisions (
    id TEXT PRIMARY KEY,                       -- tsrev_<hex>
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- A set is named directly (the run_template_revisions pattern — no lineage table); its revisions
    -- share the set_name.
    set_name TEXT NOT NULL,
    revision_number INTEGER NOT NULL,
    -- The exact pins: [{"tool_revision_id":"trev_..","overrides":{"timeout_ms":..}}]. Only PUBLISHED
    -- tool revisions may be pinned (app-enforced); a per-pin override may only TIGHTEN a declared limit
    -- (a stricter timeout), never widen it (spec §28.4 approval-only-stricter).
    tool_pins JSONB NOT NULL,
    digest TEXT NOT NULL,
    published_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, set_name, revision_number)
);

-- AgentRevision + RunTemplateRevision E12 riders (the 000019 rider idiom; NULL = the field is absent,
-- matching the tools-ceiling nil-semantics). T2 CONSUMES only tool_sets (resolves + advertises); the
-- other three ride OPAQUE — validated/consumed by their owning task (mcp_connections T5, skills T7,
-- hooks T8) — so wave-2 never has to touch agents.go / this migration again (the conflict shield).
ALTER TABLE agent_revisions
    ADD COLUMN IF NOT EXISTS tool_sets JSONB,
    ADD COLUMN IF NOT EXISTS mcp_connections JSONB,
    ADD COLUMN IF NOT EXISTS skills JSONB,
    ADD COLUMN IF NOT EXISTS hooks JSONB;
ALTER TABLE run_template_revisions
    ADD COLUMN IF NOT EXISTS tool_sets JSONB,
    ADD COLUMN IF NOT EXISTS mcp_connections JSONB,
    ADD COLUMN IF NOT EXISTS skills JSONB,
    ADD COLUMN IF NOT EXISTS hooks JSONB;

-- DML-granted to the application role explicitly (000001's blanket GRANT predates these tables and
-- re-runs each Migrate). UPDATE stays granted for the publish flip; DELETE for draft GC/retention.
GRANT SELECT, INSERT, UPDATE, DELETE ON tools, tool_revisions, tool_set_revisions TO palai_app;

INSERT INTO schema_migrations (version) VALUES (24) ON CONFLICT DO NOTHING;
