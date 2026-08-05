package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/adapters/sandboxes/oci/snapshot"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// MaxSnapshotArchiveBytes bounds a workspace snapshot archive. It matches the object store's read bound
// (artifacts.maxReadBytes) so a snapshot that could never be GET back for a restore is rejected at
// capture rather than written and later un-restorable. A var, not a const, so a deployment can tune it
// (and a test can lower it to drive the overflow path without a 64 MiB fixture). ponytail: 64 MiB covers
// the local-tier repos; a larger repo needs streaming archive/restore — this buffers the whole archive.
var MaxSnapshotArchiveBytes int64 = 64 * 1024 * 1024

// ErrSnapshotArchiveMissing reports that a snapshot row exists but its byte-archive is absent from the
// object store (a manifest-only E09 snapshot, or lost bytes): the restore has nothing to reconstruct
// from, so a recovering workspace fails EXPLICITLY (recovering→failed, spec §26.3 rung 4) rather than
// resuming on an empty tree.
var ErrSnapshotArchiveMissing = errors.New("snapshot archive missing")

// SnapshotObjectStore is the object-store PUT/GET the opaque snapshot archive bytes ride. It is the
// same control-plane-only artifacts.Store the checkpoint sink uses (spec §24 — the engine never holds
// the S3 credential); declaring the interface here keeps the sink decoupled from the artifacts import.
type SnapshotObjectStore interface {
	Put(ctx context.Context, key string, body []byte) (checksum string, size int64, err error)
	Get(ctx context.Context, key string) (body []byte, found bool, err error)
}

// SnapshotSink captures + restores a workspace snapshot's BYTE-archive (spec §29.10, E10 Task 6). E09
// recorded a manifest-only snapshot; this tars the allocation, PUTs it under a tenant-scoped
// snapshots/<id> key, and records the row with the object key (guarded by the allocation's fence
// currency, so a stale host's snapshot is rejected, SAN-006). Restore fetches the bytes and verifies
// the restored tree re-derives EQUAL create-side checksums (SAN-005).
type SnapshotSink struct {
	store SnapshotObjectStore
	spine *coordinator.Store
}

// NewSnapshotSink binds the object store and the durable spine.
func NewSnapshotSink(store SnapshotObjectStore, spine *coordinator.Store) *SnapshotSink {
	return &SnapshotSink{store: store, spine: spine}
}

// SnapshotCaptureInput is one allocation to snapshot: its tenant, the logical workspace + physical
// allocation ids, the on-host allocation directory to archive, and the reason (e.g. a pause boundary).
type SnapshotCaptureInput struct {
	SnapshotID   string
	Project      string
	WorkspaceID  string
	AllocationID string
	HostPath     string
	Reason       string
	// Ops is the filesystem HostPath is a path ON (Faz A.5 T5). Nil means this process's own, which is
	// what every caller meant before a lease could place an allocation on another Mac and is still right
	// for the deterministic tiers and for a control plane serving its own runs. A caller inside an attempt
	// passes the attempt's ops, so the tar is produced by whoever is holding the bytes.
	//
	// IT IS A FIELD RATHER THAN A SECOND FUNCTION because the two differ in ONE step — who reads the tree
	// — and everything after it (the size bound, the object key, the fence-guarded row) must not fork. A
	// captureRemote() would be a second answer to "what is a snapshot", and this file exists to have one.
	Ops toolbroker.WorkspaceOps
}

// Capture archives the allocation at HostPath (INCLUDING .git, secrets excluded — SAN-005), size-bounds
// the archive BEFORE the PUT (so an oversize snapshot leaves no orphan object), stores the bytes, and
// records the immutable row. The row insert is fence-guarded: a stale allocation (a host move advanced
// the fence) affects zero rows and returns coordinator.ErrStaleAllocation — the DB-level SAN-006 reject.
// It returns the snapshot id the checkpoint boundary links (spec §26.4).
func (s *SnapshotSink) Capture(ctx context.Context, in SnapshotCaptureInput) (string, error) {
	body, manifest, err := archiveAllocation(ctx, in.Ops, in.HostPath)
	if err != nil {
		return "", fmt.Errorf("archive workspace snapshot: %w", err)
	}
	if int64(len(body)) > MaxSnapshotArchiveBytes {
		return "", fmt.Errorf("workspace snapshot archive %d bytes exceeds the %d bound", len(body), MaxSnapshotArchiveBytes)
	}
	key := snapshotObjectKey(in.Project, in.WorkspaceID, in.SnapshotID)
	archiveChecksum, size, err := s.store.Put(ctx, key, body)
	if err != nil {
		return "", fmt.Errorf("store snapshot archive: %w", err)
	}
	fileChecksums, _ := json.Marshal(manifest.FileChecksums)
	exclusions, _ := json.Marshal(manifest.Exclusions)
	// THE ROW IS WRITTEN UNDER THE PROJECT THIS CAPTURE NAMES, taken from the input rather than from whatever
	// scope the caller's context happens to carry. SnapshotInput has no project field, so the insert lands
	// wherever the GUC points, and depending on an ambient scope for that is the fragility RestoreTo already
	// avoids by taking its tenant as an argument — this is the same guarantee for the write half.
	//
	// It is behaviour-preserving where it was already working: the orchestrator passes Project:
	// st.tenant.Project (snapshot.go's captureBoundarySnapshot) under a context scoped to that same tenant.
	// What it fixes is every OTHER caller, which had to know to scope first and got a bare
	// "tenant scope requires a project" from the pool when it did not.
	ctx = storage.ScopeToTenant(ctx, in.Project)
	if err := s.spine.CreateWorkspaceSnapshot(ctx, coordinator.SnapshotInput{
		SnapshotID:      in.SnapshotID,
		AllocationID:    in.AllocationID,
		TreeChecksum:    manifest.TreeChecksum,
		IndexChecksum:   manifest.IndexChecksum,
		FileChecksums:   fileChecksums,
		Exclusions:      exclusions,
		Reason:          in.Reason,
		ObjectKey:       key,
		ArchiveChecksum: archiveChecksum,
		SizeBytes:       size,
	}); err != nil {
		return "", err
	}
	return in.SnapshotID, nil
}

// RestoreTo fetches snapshot snapshotID's archived bytes and restores them into dest (a FRESH
// allocation dir), verifying the restored tree re-derives EQUAL create-side checksums (SAN-005). An
// absent archive is ErrSnapshotArchiveMissing and a checksum mismatch is snapshot.ErrRestoreChecksumMismatch
// — both surfaced so a recovering workspace fails explicitly rather than resuming on a wrong/empty tree.
func (s *SnapshotSink) RestoreTo(ctx context.Context, tenant coordinator.Tenant, snapshotID, dest string) (workspace.Manifest, error) {
	return s.RestoreThrough(ctx, tenant, snapshotID, dest, nil)
}

// RestoreThrough is RestoreTo onto the filesystem `ops` names (Faz A.5 T5). Nil ops is this process's
// own — RestoreTo, unchanged — and an attempt's ops put the bytes on the machine that took the lease,
// which is what lets a session resume on a Mac that is not the one it was captured on.
func (s *SnapshotSink) RestoreThrough(ctx context.Context, tenant coordinator.Tenant, snapshotID, dest string, ops toolbroker.WorkspaceOps) (workspace.Manifest, error) {
	rec, err := s.spine.LoadWorkspaceSnapshot(ctx, tenant, snapshotID)
	if err != nil {
		return workspace.Manifest{}, err
	}
	if rec.ObjectKey == "" {
		return workspace.Manifest{}, fmt.Errorf("%w: snapshot %s is manifest-only", ErrSnapshotArchiveMissing, snapshotID)
	}
	body, found, err := s.store.Get(ctx, rec.ObjectKey)
	if err != nil {
		return workspace.Manifest{}, fmt.Errorf("get snapshot archive: %w", err)
	}
	if !found {
		return workspace.Manifest{}, fmt.Errorf("%w: object %s", ErrSnapshotArchiveMissing, rec.ObjectKey)
	}
	var fileChecksums map[string]string
	_ = json.Unmarshal(rec.FileChecksums, &fileChecksums)
	want := workspace.Manifest{
		TreeChecksum:  rec.TreeChecksum,
		IndexChecksum: rec.IndexChecksum,
		FileChecksums: fileChecksums,
	}
	return restoreAllocation(ctx, ops, dest, body, want)
}

// archiveAllocation and restoreAllocation are the ONE place that decides whose disk an allocation's
// bytes are on, and they take the shape cloneOnMachine already took for the same question: a type switch
// on the attempt's ops, remote if the attempt reached a machine and this process otherwise.
//
// A NIL `ops` IS NOT A FALLBACK FOR A MACHINE THAT FAILED TO ANSWER. It is the honest answer for a
// caller that has no attempt at all — the idle releaser's sweep, the recovery ladder, the deterministic
// tiers — and for those the bytes really are here. What must never happen is a remote attempt silently
// tarring this host's filesystem, which is what a `if remote { … } else { local }` reachable from a
// failed remote call would do; the switch below cannot express that, because a remote ops that errors
// returns the error.
func archiveAllocation(ctx context.Context, ops toolbroker.WorkspaceOps, hostPath string) ([]byte, workspace.Manifest, error) {
	if remote, isRemote := ops.(*RemoteWorkspace); isRemote {
		return remote.Archive(ctx)
	}
	var buf bytes.Buffer
	manifest, err := snapshot.Archive(hostPath, &buf)
	if err != nil {
		return nil, workspace.Manifest{}, err
	}
	return buf.Bytes(), manifest, nil
}

func restoreAllocation(ctx context.Context, ops toolbroker.WorkspaceOps, dest string, body []byte, want workspace.Manifest) (workspace.Manifest, error) {
	if remote, isRemote := ops.(*RemoteWorkspace); isRemote {
		return remote.Restore(ctx, body, want)
	}
	return snapshot.Restore(bytes.NewReader(body), dest, want)
}

// captureBoundarySnapshot cuts a workspace snapshot at a pause boundary (SES-009) and returns its id for
// the checkpoint to link. It is a no-op — returns "" — when no snapshot sink is wired or the run has no
// workspace (the T4 no-dependency case): the checkpoint then declares no workspace dependency, unchanged.
// It resolves the workspace's CURRENT allocation so the snapshot is fence-guarded (a stale host's cut is
// rejected). A capture error is returned so the pause fails rather than proceeding snapshot-less (§26.5).
func (o *Orchestrator) captureBoundarySnapshot(ctx context.Context, st *attemptState) (string, error) {
	if o.snapshots == nil || st.workspaceID == "" || st.attempt.WorkspaceHostPath == "" {
		return "", nil
	}
	alloc, err := o.spine.CurrentAllocation(ctx, st.workspaceID)
	if err != nil {
		return "", err
	}
	return o.snapshots.Capture(ctx, SnapshotCaptureInput{
		SnapshotID:   "snap_" + randHex16(),
		Project:      st.tenant.Project,
		WorkspaceID:  st.workspaceID,
		AllocationID: alloc.ID,
		HostPath:     st.attempt.WorkspaceHostPath,
		Reason:       "pause",
		// THE TAR IS PRODUCED BY WHOEVER HOLDS THE BYTES (Faz A.5 T5). Until this line it was always
		// produced here, over a path that on a split deploy names nothing — so the boundary snapshot a
		// resume restores from was an archive of the control plane's empty directory. It is also the ONE
		// capture on this architecture that can reach a machine at all: the engine is still live at a pause
		// boundary (checkpointBeforePause sends checkpoint.request immediately after this), and the machine
		// stops serving ws.* the moment its engine exits (packages/runner/serve.go).
		Ops: st.workspaceOps,
	})
}

// workspaceRestorable reports whether a checkpoint's linked workspace snapshot can be restored (spec
// §26.4): it is byte-archived (a non-empty object_key). A snapshot-less checkpoint ("") is vacuously
// restorable — it declares no workspace dependency. A manifest-only snapshot (no bytes) is NOT
// restorable, so the ladder rejects the checkpoint with workspace_unrestorable rather than resuming on
// an un-rehydratable tree.
func (o *Orchestrator) workspaceRestorable(ctx context.Context, tenant coordinator.Tenant, snapshotID string) (bool, error) {
	if snapshotID == "" {
		return true, nil
	}
	rec, err := o.spine.LoadWorkspaceSnapshot(ctx, tenant, snapshotID)
	if err != nil {
		return false, err
	}
	return rec.ObjectKey != "", nil
}

// snapshotObjectKey lays out the S3 key tenant-first (defense in depth, the artifacts + checkpoints
// layout) with a snapshots/ segment so snapshot bytes never collide with artifact or checkpoint bytes.
func snapshotObjectKey(project, workspaceID, snapshotID string) string {
	return fmt.Sprintf("%s/%s/snapshots/%s", project, workspaceID, snapshotID)
}
