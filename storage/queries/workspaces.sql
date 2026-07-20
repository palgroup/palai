-- Workspace foundation queries (spec §29.7-29.10). The migration owns the constraints; these
-- statements are the write paths the coordinator store issues against them. The single-writer
-- lease and fence guards live in the SQL/constraints, not in app-code check-then-act.

-- name: CreateWorkspace
-- Open a logical workspace bound to one session (and optionally its root run). The id is the
-- stable logical lineage id; physical allocations follow separately (spec §29.7).
INSERT INTO workspaces
    (id, organization_id, project_id, session_id, run_id, state, unsafe_bind, unsafe_host_path, publication_disabled)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: AllocateWorkspace
-- Mint a new PHYSICAL allocation for a logical workspace with the next fencing token (max+1).
-- The logical workspace id is unchanged; only a new allocation id and a strictly higher fence
-- appear — the shape a host move takes (spec §29.7). UNIQUE(workspace_id, fence) makes two
-- racing allocations safe: one wins the fence, the other is a unique_violation and retries.
INSERT INTO workspace_allocations
    (id, workspace_id, organization_id, project_id, fence, host_path, state)
SELECT $1, w.id, w.organization_id, w.project_id,
       COALESCE((SELECT MAX(fence) FROM workspace_allocations WHERE workspace_id = w.id), 0) + 1,
       $3, 'active'
FROM workspaces w
WHERE w.id = $2
RETURNING id, fence;

-- name: CurrentAllocation
-- The workspace's current (max-fence) allocation — the one the supervisor mounts and the only
-- one that may upload an authoritative snapshot (spec §29.8).
SELECT id, fence, host_path
FROM workspace_allocations
WHERE workspace_id = $1
ORDER BY fence DESC
LIMIT 1;

-- name: AcquireWriterLease
-- Take the single active writer lease for a workspace, bound to an allocation and held by a run.
-- The workspace_leases_one_active_writer partial unique index rejects a second concurrent active
-- lease with 23505 — single-writer is a DB constraint, not an app race (spec §29.8). The
-- workspace, tenant, and fence are derived from the allocation so a lease cannot name a foreign one.
INSERT INTO workspace_leases
    (id, workspace_id, allocation_id, organization_id, project_id, run_id, state, fence)
SELECT $1, a.workspace_id, a.id, a.organization_id, a.project_id, $3, 'active', a.fence
FROM workspace_allocations a
WHERE a.id = $2;

-- name: ReleaseWriterLease
-- Release the lease so the single-writer slot frees for the next writer (spec §29.8).
UPDATE workspace_leases SET state = 'released' WHERE id = $1 AND state = 'active';

-- name: CreateWorkspaceSnapshot
-- Record a create-side snapshot, but ONLY when the uploading allocation is the workspace's
-- current (max-fence) allocation. A stale allocation whose fence a host move has superseded
-- affects zero rows — the DB-level reject of a stale authoritative snapshot (spec §29.8 line
-- 3070, SAN-006). The fence equality is evaluated inside the DB, so app code cannot bypass it,
-- exactly as the conditional-write fence guard on job completion does. RESTORE is E10.
INSERT INTO workspace_snapshots
    (id, workspace_id, allocation_id, organization_id, project_id, fencing_token,
     tree_checksum, index_checksum, file_checksums, exclusions, reason)
SELECT $1, a.workspace_id, a.id, a.organization_id, a.project_id, a.fence,
       $3, $4, $5, $6, $7
FROM workspace_allocations a
WHERE a.id = $2
  AND a.fence = (SELECT MAX(fence) FROM workspace_allocations WHERE workspace_id = a.workspace_id);
