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
)

// MaxSnapshotArchiveBytes bounds a workspace snapshot archive. It matches the object store's read bound
// (artifacts.maxReadBytes) so a snapshot that could never be GET back for a restore is rejected at
// capture rather than written and later un-restorable. ponytail: 64 MiB covers the local-tier repos; a
// larger repo needs streaming archive/restore (a real ceiling, named — this buffers the whole archive).
const MaxSnapshotArchiveBytes = 64 * 1024 * 1024

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
	Organization string
	Project      string
	WorkspaceID  string
	AllocationID string
	HostPath     string
	Reason       string
}

// Capture archives the allocation at HostPath (INCLUDING .git, secrets excluded — SAN-005), size-bounds
// the archive BEFORE the PUT (so an oversize snapshot leaves no orphan object), stores the bytes, and
// records the immutable row. The row insert is fence-guarded: a stale allocation (a host move advanced
// the fence) affects zero rows and returns coordinator.ErrStaleAllocation — the DB-level SAN-006 reject.
// It returns the snapshot id the checkpoint boundary links (spec §26.4).
func (s *SnapshotSink) Capture(ctx context.Context, in SnapshotCaptureInput) (string, error) {
	var buf bytes.Buffer
	manifest, err := snapshot.Archive(in.HostPath, &buf)
	if err != nil {
		return "", fmt.Errorf("archive workspace snapshot: %w", err)
	}
	if buf.Len() > MaxSnapshotArchiveBytes {
		return "", fmt.Errorf("workspace snapshot archive %d bytes exceeds the %d bound", buf.Len(), MaxSnapshotArchiveBytes)
	}
	key := snapshotObjectKey(in.Organization, in.Project, in.WorkspaceID, in.SnapshotID)
	archiveChecksum, size, err := s.store.Put(ctx, key, buf.Bytes())
	if err != nil {
		return "", fmt.Errorf("store snapshot archive: %w", err)
	}
	fileChecksums, _ := json.Marshal(manifest.FileChecksums)
	exclusions, _ := json.Marshal(manifest.Exclusions)
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
	return snapshot.Restore(bytes.NewReader(body), dest, want)
}

// snapshotObjectKey lays out the S3 key tenant-first (defense in depth, the artifacts + checkpoints
// layout) with a snapshots/ segment so snapshot bytes never collide with artifact or checkpoint bytes.
func snapshotObjectKey(org, project, workspaceID, snapshotID string) string {
	return fmt.Sprintf("%s/%s/%s/snapshots/%s", org, project, workspaceID, snapshotID)
}
