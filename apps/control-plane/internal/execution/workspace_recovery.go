package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// ErrRecoveryImpossible reports that a host-lost workspace cannot be recovered: there is no byte-archived
// snapshot to restore from, or the archive is missing/corrupt (spec §26.3 rung 4, §29.7). The workspace
// is driven recovering→failed with a typed reason rather than left dangling or resumed on an empty tree.
var ErrRecoveryImpossible = errors.New("workspace recovery impossible")

// ErrHostQuarantined reports that a placement was refused because the host was quarantined by a prior
// allocation-destroy failure (spec §29 SAN-008). A run already executing on the host is untouched; only
// a NEW allocation is denied, so a poisoned host stops accepting tenants until an operator clears it.
var ErrHostQuarantined = errors.New("host quarantined")

// WorkspaceRecovery drives a host-lost workspace back to ready on a NEW fenced allocation (spec
// §29.7-29.8, REC-005/ENG-006). It is the FIRST driver of the leased→host_lost→recovering→ready
// transitions the workspace SM has carried since E09 (packages/state-machines/workspace.go) with no
// driver. The logical workspace id is STABLE across the move; only a strictly higher allocation fence
// appears, which fences out the old host's writes/snapshots at the DB (SAN-006, already proven).
//
// CEILING — DETECTION is not yet wired into the binary. This is the recovery DRIVER (component- and
// fault-proven); nothing in the control-plane binary yet DETECTS a lost host to invoke it. The intended
// trigger is the lease-liveness path already in the tree: the durable-job lease-expiry reconcile
// (coordinator.Supervisor) noticing a workspace whose writer run has a dead response.run job
// (RunHasLiveResponseJob=false) — the same liveness signal acquireWriterLease reclaims on. Wiring that
// detect→RecoverWorkspace hook is the remaining E10 integration; until then a host-lost workspace is
// recovered lazily on the next attempt's provisionRootWorkspace (which reclaims the stale lease) rather
// than eagerly re-allocated. Named here so this reads as a driver-with-pending-trigger, not as live.
type WorkspaceRecovery struct {
	spine     *coordinator.Store
	snapshots *SnapshotSink
	root      string // the provision root new allocation dirs are minted under; also the local-tier host id
	// remove tears an allocation's host directory down. Defaulted to os.RemoveAll; a test injects a
	// failing remover to exercise the destroy-failure→quarantine path (SAN-008). ponytail: one function
	// field, not a Destroyer interface — there is exactly one real implementation.
	remove func(string) error
}

// NewWorkspaceRecovery binds the durable spine, the snapshot restore path, and the provision root (which
// doubles as the local-tier host identity for quarantine).
func NewWorkspaceRecovery(spine *coordinator.Store, snapshots *SnapshotSink, root string) *WorkspaceRecovery {
	return &WorkspaceRecovery{spine: spine, snapshots: snapshots, root: root, remove: os.RemoveAll}
}

// hostID is the local-tier host identity (the provision root). There is no hosts/runners registry in
// this tier (enrollment is cert-based, runner_gateway.go), so the provision root IS the host.
func (r *WorkspaceRecovery) hostID() string { return r.root }

// SetTeardown overrides how an allocation's host directory is reclaimed (default os.RemoveAll). It is a
// real seam — a remote/multi-host tier tears down over the wire rather than with a local RemoveAll — and
// also the point a destroy-failure fault injects through to exercise the quarantine path (SAN-008).
func (r *WorkspaceRecovery) SetTeardown(remove func(string) error) { r.remove = remove }

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
	// Split-brain-free by fencing, not by locking: two racing recoveries each mint their own allocation
	// (fence+1, fence+2). Both restore, but only the HIGHER fence is current, so the loser's later
	// writer-lease acquire hits ErrStaleAllocation and its attempt retries against the winner's
	// allocation — self-healing, no torn state. (MarkReady is idempotent on the shared workspace row.)
	for _, cmd := range []statemachines.WorkspaceCommand{statemachines.WorkspaceCmdLoseHost, statemachines.WorkspaceCmdRecover} {
		if err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, cmd); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
			return RecoverResult{}, err
		}
	}

	// Placement guard (SAN-008): never recover onto a quarantined host — checked BEFORE resolving a
	// snapshot, so a poisoned host is refused whether or not a boundary exists. A run already on it is
	// untouched; only a NEW allocation there is denied, so a poisoned host stops taking tenants.
	if quarantined, err := r.spine.IsHostQuarantined(ctx, r.hostID()); err != nil {
		return RecoverResult{}, err
	} else if quarantined {
		return RecoverResult{}, r.fail(ctx, tenant, in, fmt.Errorf("%w: %s", ErrHostQuarantined, r.hostID()))
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

// DestroyInput names the allocation to tear down: its workspace (for the SM transition) and the on-host
// directory whose bytes must be reclaimed so no tenant residue survives (SAN-007).
type DestroyInput struct {
	WorkspaceID string
	SessionID   string
	ResponseID  string
	HostPath    string
}

// DestroyAllocation tears an allocation down: it drives the workspace ready/paused/failed→destroying→
// destroyed and REMOVES the on-host directory so a later allocation on the same host inherits zero
// residue — files, credentials, or a dirty writable layer (SAN-007). If the physical teardown FAILS,
// the host is QUARANTINED (SAN-008): its bytes may still hold tenant data, so no new allocation may be
// placed there, and the failure is journaled (host.quarantined.v1). The workspace stays in destroying
// (not destroyed) — the teardown did not complete — and the typed error is returned.
func (r *WorkspaceRecovery) DestroyAllocation(ctx context.Context, tenant coordinator.Tenant, in DestroyInput) error {
	// Own the destroy before touching the filesystem. destroy is legal only from ready/paused/failed; an
	// ErrInvalidState is tolerated ONLY when the workspace is ALREADY destroying/destroyed (an idempotent
	// retry). From any live state (e.g. leased) the transition is illegal and we REFUSE — removing a live
	// workspace's bytes would be data loss, not a no-op.
	switch err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdDestroy); {
	case err == nil:
	case errors.Is(err, statemachines.ErrInvalidState):
		state, serr := r.spine.WorkspaceLifecycleState(ctx, tenant, in.WorkspaceID)
		if serr != nil {
			return serr
		}
		if state != string(statemachines.WorkspaceDestroying) && state != string(statemachines.WorkspaceDestroyed) {
			return fmt.Errorf("destroy allocation refused: workspace %s is %s, not destroyable", in.WorkspaceID, state)
		}
	default:
		return err
	}
	if err := r.remove(in.HostPath); err != nil {
		// The teardown failed — the host may still hold tenant bytes, so quarantine it and refuse future
		// placement rather than reuse a substrate we could not clean (SAN-008).
		cause := fmt.Errorf("destroy allocation %s: %w", in.HostPath, err)
		if qerr := r.spine.QuarantineHost(ctx, r.hostID(), cause.Error()); qerr != nil {
			return fmt.Errorf("%w (and quarantine failed: %v)", cause, qerr)
		}
		payload, _ := json.Marshal(map[string]any{"host_id": r.hostID(), "reason": cause.Error(), "workspace_id": in.WorkspaceID})
		if _, jerr := r.spine.RecordRecoveryEvent(ctx, tenant, in.SessionID, in.ResponseID, eventHostQuarantined, payload); jerr != nil {
			return fmt.Errorf("%w (and journal failed: %v)", cause, jerr)
		}
		return cause
	}
	if err := r.spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdFinishDestroy); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
		return err
	}
	return nil
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
