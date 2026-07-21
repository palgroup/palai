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
-- The fence-currency guard (like CreateWorkspaceSnapshot) rejects a lease on a SUPERSEDED allocation
-- once a host move has advanced the fence: a non-current allocation affects zero rows, so a stale
-- writer cannot acquire authority at the DB level (spec §29.8), not in check-then-act app code.
INSERT INTO workspace_leases
    (id, workspace_id, allocation_id, organization_id, project_id, run_id, state, fence)
SELECT $1, a.workspace_id, a.id, a.organization_id, a.project_id, $3, 'active', a.fence
FROM workspace_allocations a
WHERE a.id = $2
  AND a.fence = (SELECT MAX(fence) FROM workspace_allocations WHERE workspace_id = a.workspace_id);

-- name: ReleaseWriterLease
-- Release the lease so the single-writer slot frees for the next writer (spec §29.8).
UPDATE workspace_leases SET state = 'released' WHERE id = $1 AND state = 'active';

-- name: CreateWorkspaceSnapshot
-- Record a create-side snapshot, but ONLY when the uploading allocation is the workspace's
-- current (max-fence) allocation. A stale allocation whose fence a host move has superseded
-- affects zero rows — the DB-level reject of a stale authoritative snapshot (spec §29.8 line
-- 3070, SAN-006). The fence equality is evaluated inside the DB, so app code cannot bypass it,
-- exactly as the conditional-write fence guard on job completion does. object_key/archive_checksum/
-- size_bytes (000017) record WHERE the byte-archive lives and how to verify it (E10 Task 6, SAN-005);
-- they are '' / 0 for a manifest-only (E09) snapshot with no archived bytes.
INSERT INTO workspace_snapshots
    (id, workspace_id, allocation_id, organization_id, project_id, fencing_token,
     tree_checksum, index_checksum, file_checksums, exclusions, reason,
     object_key, archive_checksum, size_bytes)
SELECT $1, a.workspace_id, a.id, a.organization_id, a.project_id, a.fence,
       $3, $4, $5, $6, $7, $8, $9, $10
FROM workspace_allocations a
WHERE a.id = $2
  AND a.fence = (SELECT MAX(fence) FROM workspace_allocations WHERE workspace_id = a.workspace_id);

-- name: LoadWorkspaceSnapshot
-- Read a snapshot's byte-archive location + create-side manifest checksums, so a restore fetches the
-- archived bytes and verifies the restored tree re-derives EQUAL (spec §29.10, SAN-005 restore, E10
-- Task 6). Scoped by id + tenant. object_key is '' for a manifest-only (E09) snapshot with no bytes.
SELECT workspace_id, object_key, archive_checksum, size_bytes,
       tree_checksum, index_checksum, file_checksums, exclusions
FROM workspace_snapshots
WHERE id = $1 AND organization_id = $2 AND project_id = $3;

-- name: LatestRestorableWorkspaceSnapshot
-- The newest snapshot of a workspace that carries archived BYTES (object_key <> ''), so a host-lost
-- recovery restores from the most recent durable boundary (spec §29.7-29.10, REC-005, E10 Task 6). A
-- manifest-only (E09) snapshot is skipped — it has no bytes to restore. No row -> the recovery has no
-- boundary to restore from and fails EXPLICITLY (recovering→failed), never a silent empty tree.
SELECT id FROM workspace_snapshots
WHERE workspace_id = $1 AND organization_id = $2 AND project_id = $3 AND object_key <> ''
ORDER BY created_at DESC
LIMIT 1;

-- name: ReferencedSnapshotObjectKeys
-- The snapshot half of the orphan-GC reference set (E10 Task 6 <-> Task 3). Snapshot byte-archives live
-- in the SAME object-store bucket as artifacts + checkpoints under <org>/<proj>/<ws>/snapshots/<id>, but
-- are tracked HERE — so the GC must UNION these keys with the artifact + checkpoint sets, or it reclaims
-- every live snapshot as an orphan and destroys the restore bytes SAN-005 depends on. Deliberately
-- bucket-wide with NO tenant scope, matching the artifacts + checkpoints queries: the delete decision is
-- the pure absence of a referencing row, so the set must be complete across every tenant. The <> ''
-- filter skips manifest-only rows (no archived bytes to protect).
SELECT object_key FROM workspace_snapshots WHERE object_key <> '';

-- name: AttachSessionWorkspace
-- Attach a session-scoped coding workspace (spec §29.7, E09 Task 10): the logical workspace the root
-- run auto-provisions, carrying the repository binding + requested ref resolved from the POST
-- /v1/responses `repository` field. Idempotent PER SESSION — a second attach (a chained response in
-- the same session) is a no-op via WHERE NOT EXISTS, so the session keeps ONE bound workspace and its
-- edits persist across runs (the allocation is reused, not re-cloned). Runs inside the admission
-- transaction, so the workspace is attached iff the response is admitted.
INSERT INTO workspaces
    (id, organization_id, project_id, session_id, state, repository_binding_id, requested_ref)
SELECT $1, $2, $3, $4, 'requested', $5, $6
WHERE NOT EXISTS (
    SELECT 1 FROM workspaces
    WHERE session_id = $4 AND organization_id = $2 AND project_id = $3 AND repository_binding_id <> ''
);

-- name: WorkspaceForSession
-- The session's coding workspace (spec §29.7, E09 Task 10): its logical id, the bound repository +
-- requested ref, and its current lifecycle state, so the root run resolves what to provision. A session
-- with no attached binding returns no row — the run then provisions nothing (pre-E09 behaviour).
SELECT id, repository_binding_id, requested_ref, state
FROM workspaces
WHERE session_id = $1 AND organization_id = $2 AND project_id = $3 AND repository_binding_id <> ''
ORDER BY created_at
LIMIT 1;

-- name: QuarantineHost
-- Mark a host poisoned by an allocation-destroy failure (spec §29 SAN-008, E10 Task 6). Idempotent on
-- host_id — a repeat failure re-quarantines the same host without error. In the local tier the host_id
-- is the provision-root / runner identity the destroy failed under (there is no hosts registry).
INSERT INTO host_quarantine (host_id, reason) VALUES ($1, $2)
ON CONFLICT (host_id) DO NOTHING;

-- name: IsHostQuarantined
-- Whether a host is quarantined — the placement guard consults it before minting a NEW allocation on
-- that host and refuses a quarantined one (spec §29 SAN-008). A run already executing there is untouched.
SELECT EXISTS (SELECT 1 FROM host_quarantine WHERE host_id = $1);

-- name: ListQuarantinedHosts
-- Every quarantined host with its reason + time, newest first — the doctor's quarantine visibility
-- (spec §29 SAN-008). A control-plane-internal read (spec §24): host identities, no tenant data.
SELECT host_id, reason, quarantined_at FROM host_quarantine ORDER BY quarantined_at DESC;

-- name: WorkspaceState
-- The workspace's current lifecycle state within tenant scope, LOCKED for a transition (spec §29.7),
-- exactly as LockRun locks a run before a RunTable transition.
SELECT state FROM workspaces
WHERE id = $1 AND organization_id = $2 AND project_id = $3
FOR UPDATE;

-- name: UpdateWorkspaceState
-- Advance the workspace's lifecycle state projection (spec §29.7). The legal transition is checked by
-- the pure WorkspaceTable in Go before this write, exactly as UpdateRunState follows RunTable — the DB
-- stores the current state, the state machine owns legality.
UPDATE workspaces SET state = $4
WHERE id = $1 AND organization_id = $2 AND project_id = $3;
