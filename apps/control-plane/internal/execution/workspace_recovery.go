package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// ErrRecoveryImpossible reports that a host-lost workspace cannot be recovered: there is no byte-archived
// snapshot to restore from, or the archive is missing/corrupt (spec §26.3 rung 4, §29.7). The workspace
// is driven recovering→failed with a typed reason rather than left dangling or resumed on an empty tree.
var ErrRecoveryImpossible = errors.New("workspace recovery impossible")

// WorkspaceRecovery drives a host-lost workspace back to ready on a NEW fenced allocation (spec
// §29.7-29.8, REC-005/ENG-006). It is the FIRST driver of the leased→host_lost→recovering→ready
// transitions the workspace SM has carried since E09 (packages/state-machines/workspace.go) with no
// driver. The logical workspace id is STABLE across the move; only a strictly higher allocation fence
// appears, which fences out the old host's writes/snapshots at the DB (SAN-006, already proven).
type WorkspaceRecovery struct {
	spine     *coordinator.Store
	snapshots *SnapshotSink
	root      string // the provision root new allocation dirs are minted under
}

// NewWorkspaceRecovery binds the durable spine, the snapshot restore path, and the provision root.
func NewWorkspaceRecovery(spine *coordinator.Store, snapshots *SnapshotSink, root string) *WorkspaceRecovery {
	return &WorkspaceRecovery{spine: spine, snapshots: snapshots, root: root}
}

// RecoverInput names the host-lost workspace and the run/session recovering it. SnapshotID is optional:
// empty resolves the workspace's latest byte-archived snapshot (the natural boundary), a set value pins
// the checkpoint's linked snapshot (spec §26.4).
type RecoverInput struct {
	WorkspaceID string
	RunID       string
	SessionID   string
	ResponseID  string
	SnapshotID  string
}

// RecoverResult is the recovered workspace's new allocation and the restored create-side manifest — the
// evidence a host move happened (a strictly higher fence) and the restore is checksum-EQUAL (SAN-005).
type RecoverResult struct {
	Allocation coordinator.Allocation
	SnapshotID string
	Manifest   workspace.Manifest
}

// RecoverWorkspace drives leased→host_lost→recovering, mints a NEW allocation (fence+1) under a fresh
// host dir, restores the boundary snapshot into it verifying the create-side checksums (SAN-005), then
// recovering→ready and journals workspace.restored.v1. The logical id is unchanged; the old allocation
// is now a lower fence, so its writer-lease/snapshot attempts are rejected at the DB (SAN-006/ENG-007).
// If no restorable snapshot exists or the archive is missing/corrupt, it drives recovering→failed with
// a typed reason (ErrRecoveryImpossible) — never a silent drop.
func (r *WorkspaceRecovery) RecoverWorkspace(ctx context.Context, tenant coordinator.Tenant, in RecoverInput) (RecoverResult, error) {
	// leased→host_lost→recovering. A workspace already past leased (a raced double-recover) tolerates
	// the illegal transition — the physical restore below is the authority — but never skips silently.
	for _, cmd := range []statemachines.WorkspaceCommand{statemachines.WorkspaceCmdLoseHost, statemachines.WorkspaceCmdRecover} {
		if err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, cmd); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
			return RecoverResult{}, err
		}
	}

	snapshotID := in.SnapshotID
	if snapshotID == "" {
		latest, found, err := r.spine.LatestRestorableWorkspaceSnapshot(ctx, tenant, in.WorkspaceID)
		if err != nil {
			return RecoverResult{}, err
		}
		if !found {
			return RecoverResult{}, r.fail(ctx, tenant, in, fmt.Errorf("%w: no byte-archived snapshot for workspace %s", ErrRecoveryImpossible, in.WorkspaceID))
		}
		snapshotID = latest
	}

	// Mint the new allocation (fence+1, logical id stable) under a fresh host dir, then restore the
	// boundary snapshot into it. A fresh dir means zero residue from any prior tenant (SAN-007).
	allocID := "alloc_" + randHex16()
	dir := filepath.Join(r.root, allocID)
	alloc, err := r.spine.AllocateWorkspace(ctx, allocID, in.WorkspaceID, dir)
	if err != nil {
		return RecoverResult{}, err
	}
	manifest, err := r.snapshots.RestoreTo(ctx, tenant, snapshotID, dir)
	if err != nil {
		// A missing/corrupt archive is an explicit recovery failure, not a silent empty-tree resume.
		return RecoverResult{}, r.fail(ctx, tenant, in, fmt.Errorf("%w: restore snapshot %s: %v", ErrRecoveryImpossible, snapshotID, err))
	}

	if err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdMarkReady); err != nil {
		return RecoverResult{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"workspace_id":      in.WorkspaceID,
		"run_id":            in.RunID,
		"new_allocation_id": alloc.ID,
		"new_fence":         alloc.Fence,
		"snapshot_id":       snapshotID,
		"tree_checksum":     manifest.TreeChecksum,
	})
	if _, err := r.spine.RecordRecoveryEvent(ctx, tenant, in.SessionID, in.ResponseID, eventWorkspaceRestored, payload); err != nil {
		return RecoverResult{}, err
	}
	return RecoverResult{Allocation: alloc, SnapshotID: snapshotID, Manifest: manifest}, nil
}

// fail drives recovering→failed and returns the typed cause, so an unrecoverable workspace surfaces an
// explicit terminal reason (spec §26.3 rung 4) rather than lingering in recovering. An already-terminal
// workspace tolerates the illegal transition; the returned cause is still the authority the caller sees.
// No "restored" event is journaled — the workspace.failed.v1 transition is the record; misnaming a
// failure as a restore would be worse than a quiet transition.
func (r *WorkspaceRecovery) fail(ctx context.Context, tenant coordinator.Tenant, in RecoverInput, cause error) error {
	if err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdFail); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
		return err
	}
	return cause
}
