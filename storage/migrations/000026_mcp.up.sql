-- MCP connections registry (spec §28.13-28.14, E12 Task 5). An `mcp_connections` row is a durable, admin-
-- registered upstream MCP server binding a project may discover tools from. It is NEVER model-creatable —
-- create + discover are admin API actions (a test pins the absence of any model-facing MCP-add tool).
--
-- The discovered-revision link is NOT a new column: a discovered tool is a normal tool_revisions row
-- (000024) with executor='mcp' and executor_config->>'connection_id' pointing back here, so a connection's
-- tools ride the SAME immutable-revision + publish + digest discipline as every other registry tool. A
-- description/annotation change on re-discovery is a NEW draft revision (000024's UNIQUE(tool_id,
-- revision_number)) — published stays, re-approval required (EXT-006). This migration adds no tool columns.
--
-- Secret hygiene: config carries only NON-secret wiring (stdio: image_digest + cmd; http: url); a bearer is
-- a secret_ref HANDLE resolved from an env file bridge at REQUEST time (webhookSecretResolver pattern),
-- never inline bytes. trust_level is explicit (§28.13); the capability ceiling is the rider intersection
-- (a run reaches a connection's tools only if its AgentRevision.mcp_connections names it — 000024 rider).
-- Every CREATE ... IF NOT EXISTS keeps the migration idempotent (Migrate re-runs per boot).

CREATE TABLE IF NOT EXISTS mcp_connections (
    id TEXT PRIMARY KEY,                            -- mcpc_<hex>
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The connection name is the discovery namespace segment: a discovered tool's canonical name is
    -- mcp.<name>.<remote_tool>, so two servers' `search` tools can never collide. Unique per project.
    name TEXT NOT NULL,
    -- 'stdio' | 'http'. App-validated; NO CHECK (the 000024 executor pattern — a new transport needs no
    -- migration).
    transport TEXT NOT NULL,
    -- Non-secret wiring only. stdio: {"image_digest":"sha256:..","cmd":["/mcp"]}; http: {"url":"https://.."}.
    -- A credential is NEVER stored here — it is a secret_ref handle (below).
    config JSONB NOT NULL,
    -- The bearer/credential HANDLE (an env-file bridge key, resolved at request time). NULL = no auth.
    secret_ref TEXT,
    -- Explicit trust level (§28.13); 'untrusted' is the safe default — a discovered description is never
    -- trusted, and the capability ceiling is the rider intersection regardless of this value.
    trust_level TEXT NOT NULL DEFAULT 'untrusted',
    -- Admin kill-switch: a disabled connection resolves NO tools (the breaker stays in-memory; this is the
    -- durable off-switch). NULL = enabled.
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

-- DML-granted to the application role explicitly (000001's blanket GRANT predates this table). UPDATE for
-- the disabled_at flip; DELETE for admin removal.
GRANT SELECT, INSERT, UPDATE, DELETE ON mcp_connections TO palai_app;

INSERT INTO schema_migrations (version) VALUES (26) ON CONFLICT DO NOTHING;
