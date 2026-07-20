-- Logical Workspace / Binding / physical Allocation, single-writer lease, and create-side
-- snapshot (spec §29.7-29.10). A Workspace is a process/host-independent logical filesystem
-- lineage; its id is STABLE across host movement. A WorkspaceBinding connects it to one
-- session/run and carries the §29.7 lifecycle state (requested→provisioning→preparing→ready→
-- leased + snapshotting/paused/restoring/host_lost/recovering/failed/destroying/destroyed),
-- driven by the pure packages/state-machines WorkspaceTable — the DB stores the current state,
-- exactly as runs.state stores the RunState. Each PHYSICAL allocation carries its own allocation
-- id and a monotonic fencing token; a host move mints a new allocation (higher fence) without
-- changing the logical workspace id.
--
-- Every CREATE ... IF NOT EXISTS keeps the migration idempotent (Migrate is re-run per boot),
-- matching the 000005/000007 pattern. Tenant scope is the composite (organization_id, project_id)
-- FK to projects every execution row carries (spec §39.2); the new tables are granted to the
-- palai_app role explicitly (000001's blanket GRANT covered only the tables that existed then).

CREATE TABLE IF NOT EXISTS workspaces (
    -- The LOGICAL workspace id: stable across host movement (spec §29.7). Physical allocations
    -- come and go beneath it; this id does not.
    id TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The binding: a workspace connects to one session (and optionally its root run, §29.7).
    session_id TEXT NOT NULL REFERENCES sessions (id),
    run_id TEXT REFERENCES runs (id),
    -- The WorkspaceBinding lifecycle state (spec §29.7). Legal transitions are enforced by the
    -- pure WorkspaceTable in Go; this column is the current projection.
    state TEXT NOT NULL DEFAULT 'requested',
    -- Unsafe local bind (spec §30.13, REP-012): a direct mutable bind mount is opt-in only via an
    -- explicit unsafe-development flag. When set, the exact host scope is recorded and publication
    -- is disabled — sandbox isolation cannot be claimed. Default is the safe snapshot/copy mode.
    unsafe_bind BOOLEAN NOT NULL DEFAULT false,
    unsafe_host_path TEXT NOT NULL DEFAULT '',
    publication_disabled BOOLEAN NOT NULL DEFAULT false,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE IF NOT EXISTS workspace_allocations (
    -- A NEW allocation id per physical allocation (spec §29.7 last paragraph). Distinct from the
    -- logical workspace id, which is stable.
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The fencing token: strictly increasing per workspace (spec §29.7). The allocation with the
    -- MAX fence is the current, authoritative one; a lower fence is a stale host (§29.8 line 3070).
    fence BIGINT NOT NULL,
    -- The opaque host directory bind-mounted to /workspace inside the sandbox. Hidden from the
    -- model (§29.9 — exact host paths are hidden); the supervisor mounts it, the engine never
    -- learns it.
    host_path TEXT NOT NULL DEFAULT '',
    state TEXT NOT NULL DEFAULT 'active',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    -- A fence is never reused for a workspace: a host move only ever mints a strictly higher one,
    -- so (workspace_id, fence) is unique. This is the DB half of the AcceptFence guard.
    UNIQUE (workspace_id, fence),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

CREATE TABLE IF NOT EXISTS workspace_leases (
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    allocation_id TEXT NOT NULL REFERENCES workspace_allocations (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The writer: the root run normally holds the single active lease (spec §29.8). A child run
    -- takes a read-only snapshot / isolated COW branch instead (enforced in E09 Task 6).
    run_id TEXT NOT NULL REFERENCES runs (id),
    state TEXT NOT NULL DEFAULT 'active',
    fence BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- Single-writer lease (spec §29.8): a mutable workspace has AT MOST ONE active writer lease. A
-- second concurrent active lease is a unique_violation (23505) at the DB, not an app-code
-- check-then-insert race — the exact one-active-root / active-attempt-fence partial-unique-index
-- pattern (spec §22.3). The slot frees when the holder releases (state <> 'active').
CREATE UNIQUE INDEX IF NOT EXISTS workspace_leases_one_active_writer
    ON workspace_leases (workspace_id)
    WHERE state = 'active';

CREATE TABLE IF NOT EXISTS workspace_snapshots (
    -- Create-side snapshot metadata (spec §29.10). RESTORE and the incremental parent chain are
    -- E09's out-of-scope half (E10) — this records the create-side manifest only.
    id TEXT PRIMARY KEY,
    workspace_id TEXT NOT NULL REFERENCES workspaces (id),
    allocation_id TEXT NOT NULL REFERENCES workspace_allocations (id),
    organization_id TEXT NOT NULL,
    project_id TEXT NOT NULL,
    -- The uploading allocation's fence. A snapshot may only be recorded by the CURRENT allocation
    -- (max fence); the guarded insert in queries/workspaces.sql rejects a stale writer after
    -- fencing advances (spec §29.8 line 3070, SAN-006). Real host-kill fault is E10.
    fencing_token BIGINT NOT NULL,
    -- Content-addressed integrity over the worktree and index, plus the per-file checksum map the
    -- manifest reconstructs from (spec §29.10). Computed create-side over the real allocation FS.
    tree_checksum TEXT NOT NULL,
    index_checksum TEXT NOT NULL DEFAULT '',
    file_checksums JSONB NOT NULL DEFAULT '{}',
    -- The excluded-path manifest (spec §29.10): secret mounts, credential helpers, and /secrets
    -- never enter the snapshot. Recreated empty on restore (SAN-005).
    exclusions JSONB NOT NULL DEFAULT '[]',
    reason TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (organization_id, project_id) REFERENCES projects (organization_id, id)
);

-- The new tables are DML-granted to the application role (000001's GRANT ON ALL TABLES only
-- covered tables that existed then; a security boundary, so it is explicit here, not inherited).
GRANT SELECT, INSERT, UPDATE, DELETE ON workspaces, workspace_allocations, workspace_leases, workspace_snapshots TO palai_app;

INSERT INTO schema_migrations (version) VALUES (8) ON CONFLICT DO NOTHING;
