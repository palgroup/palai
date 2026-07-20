package execution

import (
	"context"
	"errors"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// SetWorkspaceProvisioner wires the root run's workspace auto-provisioning (spec §29.7-30.3, E09 Task
// 10): root is the host directory allocations are minted under (PALAI_WORKSPACE_ROOT), and broker mints
// the short-lived read credential the clone runs behind. Left unset, a run with an attached binding
// simply gets no workspace — its coding tools then fail cleanly (no host path), the SetShellRunner
// discipline. main.go wires it env-gated where a sandbox is configured.
func (o *Orchestrator) SetWorkspaceProvisioner(root string, broker repositories.Broker) {
	o.provisionRoot, o.provisionBroker = root, broker
}

// provisionRootWorkspace realizes the session's attached coding workspace for the ROOT run and returns
// the allocation host path (the tools' WorkspaceRoot; the repo lives at hostPath/repo), the writer lease
// to release at attempt end, and the logical workspace id. It drives the §29.7 lifecycle
// requested→provisioning→preparing→ready→leased: on the FIRST run it allocates a host dir, lays out the
// workspace, and clones @ the requested ref under a brokered credential (CP-side, the model never sees
// it); a LATER run in the same session reuses the current allocation — edits persist, the clone is not
// repeated — and records a fresh preparation receipt at the current head so its changeset diffs from
// where this run starts. Either way it acquires the single writer lease (spec §29.8). A session with no
// attachment (found=false) yields "" — the run then has no workspace, exactly as before.
//
// ponytail: this handles the clean happy path + pause/resume (the defer release returns the workspace to
// ready, so resume re-leases the SAME allocation). Reclaim after a HARD worker kill — a dangling active
// lease on a still-`leased` workspace — is E10 recovery (the host_lost/recovering states exist for it);
// here it surfaces as a lease conflict routed to retry, not silent corruption.
func (o *Orchestrator) provisionRootWorkspace(ctx context.Context, tenant coordinator.Tenant, sessionID, runID string, fence uint64) (hostPath, leaseID, workspaceID string, err error) {
	ws, found, err := o.spine.WorkspaceForSession(ctx, tenant, sessionID)
	if err != nil || !found {
		return "", "", "", err
	}

	var alloc coordinator.Allocation
	switch statemachines.WorkspaceState(ws.State) {
	case statemachines.WorkspaceReady, statemachines.WorkspaceLeased:
		// A later run in the session: ready = released by a prior run; leased = a prior attempt whose
		// state-release lost the race to a crash. Reuse the current allocation — edits persist, the clone
		// is not repeated.
		alloc, err = o.reuseAllocation(ctx, tenant, ws, runID)
	default:
		// requested, or provisioning/preparing left by a crashed/failed clone (blocker 2): (re)provision
		// fresh and idempotently — a partial allocation from a failed attempt is abandoned, a new one cloned.
		alloc, err = o.provisionFreshAllocation(ctx, tenant, ws, runID, fence)
	}
	if err != nil {
		return "", "", "", err
	}

	// The single writer lease the root run holds for the whole run (spec §29.8), released at attempt end.
	leaseID = "lease_" + randHex16()
	if err := o.spine.AcquireWriterLease(ctx, leaseID, alloc.ID, runID); err != nil {
		return "", "", "", err
	}
	// Drive ready→leased. A workspace already `leased` (the crash inconsistency above) has no Lease
	// transition, so ErrInvalidState is tolerated — the physical lease we just acquired is the authority.
	// Any OTHER failure would LEAK the just-acquired lease (no TTL until E10 recovery, so the session
	// bricks forever — blocker 1), because the caller's defer release is not armed until this returns a
	// leaseID; release it here before surfacing the error.
	if err := o.spine.AdvanceWorkspace(ctx, tenant, ws.WorkspaceID, statemachines.WorkspaceCmdLease); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
		_ = o.spine.ReleaseWriterLease(context.Background(), leaseID)
		return "", "", "", err
	}
	return alloc.HostPath, leaseID, ws.WorkspaceID, nil
}

// provisionFreshAllocation mints the first physical allocation for a workspace: a host dir under the
// provisioner root, the §29.9 workspace layout, then the deterministic clone @ the requested ref (the
// preparation receipt is the model-independent provenance). It drives requested→provisioning→preparing→
// ready around the clone.
func (o *Orchestrator) provisionFreshAllocation(ctx context.Context, tenant coordinator.Tenant, ws coordinator.SessionWorkspace, runID string, fence uint64) (coordinator.Allocation, error) {
	allocID := "alloc_" + randHex16()
	dir := filepath.Join(o.provisionRoot, allocID)
	if err := workspace.Prepare(dir); err != nil {
		return coordinator.Allocation{}, err
	}
	// Drive requested→provisioning→preparing idempotently: a retry after a failed clone re-enters from
	// `provisioning` or `preparing`, so an already-applied transition (ErrInvalidState) is skipped —
	// mirroring the run-transition loop in ExecuteAttempt. This is what lets a stuck-mid-provision
	// workspace recover instead of bricking (blocker 2).
	for _, cmd := range []statemachines.WorkspaceCommand{statemachines.WorkspaceCmdProvision, statemachines.WorkspaceCmdPrepare} {
		if err := o.spine.AdvanceWorkspace(ctx, tenant, ws.WorkspaceID, cmd); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
			return coordinator.Allocation{}, err
		}
	}
	alloc, err := o.spine.AllocateWorkspace(ctx, allocID, ws.WorkspaceID, dir)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	// The infrastructure-owned clone @ the exact ref, under a brokered read credential the model never
	// sees (spec §30.2-30.3). This is the exact call the repository.go deferral named — now wired.
	if _, err := PrepareRepository(ctx, o.spine, o.provisionBroker, tenant, PrepareRepositoryInput{
		BindingID:    ws.BindingID,
		RunID:        runID,
		RequestedRef: ws.RequestedRef,
		WorkBranch:   rootWorkBranch(ws.WorkspaceID, runID),
		TargetDir:    filepath.Join(dir, workspace.RepoDir),
		SecretsDir:   filepath.Join(dir, provisionSecretsDir),
		AttemptFence: fence,
		ToolCall:     "provision",
	}); err != nil {
		return coordinator.Allocation{}, err
	}
	if err := o.spine.AdvanceWorkspace(ctx, tenant, ws.WorkspaceID, statemachines.WorkspaceCmdMarkReady); err != nil {
		return coordinator.Allocation{}, err
	}
	return alloc, nil
}

// reuseAllocation reuses the session workspace's current allocation for a LATER run — the clone is not
// repeated, so a prior run's edits persist (spec §29.7, E09 Task 10) — and records a fresh preparation
// receipt at the current head so THIS run's changeset diffs from where it starts, not the original clone.
func (o *Orchestrator) reuseAllocation(ctx context.Context, tenant coordinator.Tenant, ws coordinator.SessionWorkspace, runID string) (coordinator.Allocation, error) {
	alloc, err := o.spine.CurrentAllocation(ctx, ws.WorkspaceID)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	head, tree, err := repositories.Head(ctx, filepath.Join(alloc.HostPath, workspace.RepoDir))
	if err != nil {
		return coordinator.Allocation{}, err
	}
	if err := o.spine.RecordPreparationReceipt(ctx, tenant, coordinator.PreparationReceiptInput{
		ReceiptID:    "prep_" + randHex16(),
		BindingID:    ws.BindingID,
		RunID:        runID,
		RequestedRef: ws.RequestedRef,
		BaseCommit:   head,
		TreeHash:     tree,
		Branch:       rootWorkBranch(ws.WorkspaceID, runID),
	}); err != nil {
		return coordinator.Allocation{}, err
	}
	return alloc, nil
}

// releaseWorkspace frees the root run's writer lease and returns the workspace to ready at attempt end
// (spec §29.8). It runs on a fresh context so a canceled/teardown ctx cannot skip the release, and is a
// no-op when the attempt provisioned no workspace (leaseID empty).
func (o *Orchestrator) releaseWorkspace(tenant coordinator.Tenant, workspaceID, leaseID string) {
	if leaseID == "" {
		return
	}
	ctx := context.Background()
	_ = o.spine.ReleaseWriterLease(ctx, leaseID)
	_ = o.spine.AdvanceWorkspace(ctx, tenant, workspaceID, statemachines.WorkspaceCmdRelease)
}

// provisionSecretsDir is the snapshot-excluded /secrets staging area under an allocation the credential
// helper + git home live in (spec §29.10) — a sibling of the engine-visible repo dir, never inside it.
const provisionSecretsDir = "secrets"

// rootWorkBranch is the root run's generated work branch: agent/<workspace>/<run>. It is recorded on the
// preparation receipt, so the approved push (T9) targets exactly this branch — resolved from the binding
// + receipt, never the model.
func rootWorkBranch(workspaceID, runID string) string {
	return "agent/" + workspaceID + "/" + runID
}
