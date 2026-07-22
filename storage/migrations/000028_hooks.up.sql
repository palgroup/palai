-- Hooks registry (spec §28.17, E12 Task 8, TOL-012). A `hooks` row is a durable, admin-registered
-- extension point that fires INSIDE the run's single dispatch loop at one of five pinned points
-- (before_tool / after_tool / before_model / on_terminal / before_repository_publish). It is NEVER
-- model-creatable — create + disable are admin API actions (a test pins the absence of any model-facing
-- hook-register tool), and a hook output can never grant a capability (the transform patch schema carries
-- no tools/model/budget/secret field).
--
-- Execution model (§28.17): tenant hook code runs in the API process NEVER — a remote_http hook is the
-- SAME T4 signed remote-worker invoke the registry tools use (transport reuse; wasm sandbox is E16+). A
-- platform_inline hook is a platform-authored, deterministic, network-less handler named by config.handler.
-- Category drives the fail-mode: policy = sync fail-CLOSED (a deny blocks the guarded operation, visibly);
-- transform = a schema-validated patch to before_tool.arguments / after_tool.result, fail-CLOSED; observer =
-- async fail-OPEN (a crash never affects the operation). The (category × point) matrix is APP-validated
-- (extensions.DecodeHookInput against hookMatrix), NOT a CHECK — a new point/category needs no migration (the
-- 000024 executor pattern).
--
-- Secret hygiene: config carries only NON-secret wiring (remote: {"url":..,"allow_private"?}; inline:
-- {"handler":..}); a signing credential is a secret_ref HANDLE resolved fresh per invoke from the org-scoped
-- file bridge (the remote_http tool pattern), never inline bytes. Every CREATE ... IF NOT EXISTS keeps the
-- migration idempotent (Migrate re-runs per boot). Firing order is (created_at, id) — documented, no position
-- column: two hooks at the same point fire in registration order.

CREATE TABLE IF NOT EXISTS hooks (
    id TEXT PRIMARY KEY,                             -- hook_<hex>
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- Unique per project. A hook is identified by its name within a project (the admin management key).
    name TEXT NOT NULL,
    -- One of the five pinned points. App-validated closed set; NO CHECK (the 000024/000026 pattern — a new
    -- point needs no migration).
    hook_point TEXT NOT NULL,
    -- 'policy' | 'transform' | 'observer'. App-validated against the (category × point) matrix; NO CHECK.
    category TEXT NOT NULL,
    -- 'platform_inline' | 'remote_http'. App-validated; NO CHECK. platform_inline names a code-defined
    -- deterministic handler; remote_http reuses the T4 signed transport.
    executor TEXT NOT NULL,
    -- Non-secret wiring only. remote_http: {"url":"https://..","allow_private":false}; platform_inline:
    -- {"handler":"deny_tool"}. A credential is NEVER stored here — it is a secret_ref handle (below).
    config JSONB NOT NULL,
    -- The signing-credential HANDLE for a remote_http hook (an env-file bridge key, resolved fresh per
    -- invoke). NULL = no secret (a platform_inline hook, or an unsigned remote hook — rejected at dispatch).
    secret_ref TEXT,
    -- Per-hook timeout hint; the dispatcher CLAMPS it to the category ceiling (policy/transform small,
    -- observer generous), so a hook can never pin a dispatch slot beyond its ceiling. NULL = category default.
    timeout_ms INTEGER,
    -- Admin kill-switch: a disabled hook never fires (the durable off-switch; the per-hook breaker is
    -- in-memory). NULL = enabled.
    disabled_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id),
    UNIQUE (organization_id, project_id, name)
);

-- Firing order is (created_at, id): a per-project, per-point read walks hooks in registration order, so the
-- index that serves the dispatch load also serves the deterministic order.
CREATE INDEX IF NOT EXISTS hooks_point_order_idx
    ON hooks (organization_id, project_id, hook_point, created_at, id);

-- DML-granted to the application role explicitly (000001's blanket GRANT predates this table). UPDATE for
-- the disabled_at flip; DELETE for admin removal.
GRANT SELECT, INSERT, UPDATE, DELETE ON hooks TO palai_app;

INSERT INTO schema_migrations (version) VALUES (28) ON CONFLICT DO NOTHING;
