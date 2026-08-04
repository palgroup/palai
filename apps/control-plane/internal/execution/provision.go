package execution

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// SetWorkspaceProvisioner wires the root run's workspace auto-provisioning (spec §29.7-30.3, E09 Task
// 10): root is the host directory allocations are minted under (PALAI_WORKSPACE_ROOT), and broker mints
// the short-lived read credential the clone runs behind. Left unset, a run with an attached binding
// simply gets no workspace — its coding tools then fail cleanly (no host path), the SetBackgroundRunner
// discipline. main.go wires it env-gated where a sandbox is configured.
func (o *Orchestrator) SetWorkspaceProvisioner(root string, broker repositories.Broker) {
	o.provisionRoot, o.provisionBroker = root, broker
}

// SetSessionAccounts wires the per-allocation uid (macOS). Unset, an allocation gets no account and its
// tools run as the control plane's own uid — which is what every deployment does today and is the
// posture docs/operations/palai-on-a-mac.md §1 describes as "the boundary is the uid. Nothing else." A
// setter rather than a constructor argument for the same reason SetBackgroundRunner is one: the composition
// root decides, and every existing caller compiles and behaves unchanged.
func (o *Orchestrator) SetSessionAccounts(a SessionAccounts) { o.sessionAccounts = a }

// SetConnectionSecrets wires the resolver a binding's connection_ref is redeemed through (E13 Task 9),
// so a tenant's own Git credential — provisioned and rotated over the secret-ref API — backs the clone
// for the bindings that name one. main.go calls it unconditionally next to SetWorkspaceProvisioner: any
// composition root that provisions workspaces MUST wire it, because a ref-bearing binding fails closed
// without it rather than quietly borrowing the deployment-global credential.
//
// UPGRADING FROM PRE-T9: connection_ref had no reader, so a ref-bearing binding cloned with the global
// GitHub App credential. It now has to resolve — the ref must exist under the binding's organization and
// the deployment must have a secret master key — or the clone fails. A binding that really does want the
// deployment credential carries an EMPTY connection_ref.
func (o *Orchestrator) SetConnectionSecrets(secrets SecretResolver) {
	o.provisionSecrets = secrets
}

// PROVISIONING IS TWO HALVES NOW, AND THE SEAM BETWEEN THEM IS THE DIAL (A.3 T5).
//
// It used to be one function that ran before the dial and did everything on THIS host: mkdir, clone,
// state machine, writer lease. That was correct while the bytes lived here and became wrong the
// moment a lease could place a run on another machine — the control plane would clone into its own
// filesystem and hand the machine a path that means nothing there.
//
// planRootWorkspace runs BEFORE the dial because the lease offer carries the allocation path: the
// runner bind-mounts it and, since this task, CREATES it (packages/runner/serve.go). It reads and
// mints, and writes nothing. realizeRootWorkspace runs AFTER, on the connection the dial returned,
// so every byte it puts on disk goes to the machine that will run the commands.
//
// WHY NOT MINT THE PATH ON THE MACHINE TOO. It would be one fewer thing the control plane names, and
// it cannot happen before the dial, and the mount needs it AT the offer. The name is therefore still
// this side's, and the machine's defence is unchanged and structural: it refuses any path outside the
// root it was configured with, and now refuses to CREATE one too. That both ends resolve
// PALAI_WORKSPACE_ROOT to the same absolute path is not a new coupling this introduces — it is the
// requirement cmd/cli/internal/stack/native.go already calls "the ONE absolute path a run's workspace
// has on both sides".

// workspacePlan is what the pre-dial half decided: which workspace the session has, which allocation
// this run will use, and whether that allocation still needs a clone.
type workspacePlan struct {
	ws        coordinator.SessionWorkspace
	sessionID string
	allocID   string
	hostPath  string
	// fresh distinguishes the two paths realizeRootWorkspace takes. A fresh allocation is cloned @ the
	// requested ref; a reused one is not — a prior run's edits persist and only a new preparation
	// receipt at the current head is recorded, so this run's changeset diffs from where it starts.
	fresh bool
}

// planRootWorkspace decides the session's coding workspace for the ROOT run without touching disk or
// writing a row. A session with no attachment yields found=false — the run then has no workspace,
// exactly as before.
func (o *Orchestrator) planRootWorkspace(ctx context.Context, tenant coordinator.Tenant, sessionID string) (workspacePlan, bool, error) {
	ws, found, err := o.spine.WorkspaceForSession(ctx, tenant, sessionID)
	if err != nil || !found {
		return workspacePlan{}, false, err
	}
	switch statemachines.WorkspaceState(ws.State) {
	case statemachines.WorkspaceReady, statemachines.WorkspaceLeased:
		// A later run in the session: ready = released by a prior run; leased = a prior attempt whose
		// state-release lost the race to a crash. Reuse the current allocation.
		alloc, err := o.spine.CurrentAllocation(ctx, ws.WorkspaceID)
		if err != nil {
			return workspacePlan{}, false, err
		}
		return workspacePlan{ws: ws, sessionID: sessionID, allocID: alloc.ID, hostPath: alloc.HostPath}, true, nil
	case statemachines.WorkspacePaused, statemachines.WorkspaceRestoring:
		// THE MACHINE WAS HANDED BACK WHILE THE THREAD WAS QUIET. An idle releaser archives the
		// allocation and reclaims its directory, and a woken thread has to restore it onto a new fenced
		// allocation — which may well be under a different provision root on a different Mac.
		//
		// NOTHING PAUSES A WORKSPACE ON `main` TODAY, and this arm says so rather than pretending
		// otherwise: the releaser and ResumeReleasedWorkspace live on wip/workspace-idle-release and are
		// A.4's work. The arm exists because the SWITCH is written once — the parked branch touches these
		// exact lines — and because falling into `default` here would silently clone a FRESH allocation
		// over a workspace whose bytes are in an archive, which is the loudest possible way to lose a
		// user's work quietly. It refuses instead.
		return workspacePlan{}, false, fmt.Errorf("workspace %s is %s: restoring a released workspace is not wired on this build",
			ws.WorkspaceID, ws.State)
	case statemachines.WorkspaceSnapshotting:
		// An idle sweep died between claiming the workspace and finishing with it. The bytes are still on
		// disk — a releaser deletes them only after reaching `paused` — so the repair is to give the claim
		// back and reuse the allocation, NOT to restore. Restoring would replace a tree that is present
		// and current with an archive that may be older than it. The claim-back is A.4's, with the sweep
		// that makes the claim; until then this refuses rather than reusing a workspace another party
		// believes it holds.
		return workspacePlan{}, false, fmt.Errorf("workspace %s is snapshotting: another party holds it", ws.WorkspaceID)
	default:
		// requested, or provisioning/preparing left by a crashed/failed clone (blocker 2): (re)provision
		// fresh and idempotently — a partial allocation from a failed attempt is abandoned, a new one
		// cloned.
		allocID := "alloc_" + randHex16()
		return workspacePlan{ws: ws, sessionID: sessionID, allocID: allocID, hostPath: filepath.Join(o.provisionRoot, allocID), fresh: true}, true, nil
	}
}

// realizeRootWorkspace puts the workspace on the machine and returns the allocation root the tools
// confine to, the writer lease to release at attempt end, and the logical workspace id.
//
// EVERY BYTE IT WRITES GOES THROUGH `ops`, which is the whole of this task at the control plane: for a
// channel that reaches a machine that is the machine's disk, and for one that does not it is this
// host's — the deterministic e2e tier, and the hand-built component orchestrators, which have no
// machine at all. It derives them from the CHANNEL, which is what every later tool call does too
// (tool_dispatch.go execEnv), so provisioning and the tools cannot end up on two different hosts.
func (o *Orchestrator) realizeRootWorkspace(ctx context.Context, ch EngineChannel, tenant coordinator.Tenant, plan workspacePlan, runID, jobID string, fence uint64) (hostPath, leaseID, workspaceID string, err error) {
	ws := plan.ws
	ops := workspaceOpsFor(ch, plan.hostPath)

	// THE ALLOCATION IS OPENED BY WHOEVER HOLDS THE DISK, which is the one line where the two cases
	// genuinely differ and so the one place that names them. Both are idempotent — a later run in the
	// same session re-opens an allocation full of a prior run's edits and must keep them — which is why
	// neither is conditional on `fresh`.
	if remote, isRemote := ops.(*RemoteWorkspace); isRemote {
		// The runner already laid the directory out when it admitted the lease, so this asks for one
		// thing: the PATH. The machine resolves it against its own root, and on macOS a /var allocation
		// comes back /private/var. Storing the unresolved name would put a path in the allocation row
		// that every later under-root check has to re-resolve, and this tree's record on path comparisons
		// is that the version resolving once and keeping the answer is the one that stays correct.
		root, oerr := remote.Open(ctx)
		if oerr != nil {
			return "", "", "", fmt.Errorf("open allocation on the machine: %w", oerr)
		}
		plan.hostPath = root
	} else if err := workspace.Prepare(plan.hostPath); err != nil {
		// No machine, so this host holds the disk and this host lays it out — which is exactly what
		// provisionFreshAllocation did before the split. It is NOT optional and it is not only about the
		// repo: repositories.Prepare creates its own target dir, but scratch, artifacts and the skills
		// tree have no other author, and NewWorkspaceFS refuses a root that is not there at all.
		return "", "", "", fmt.Errorf("lay out allocation %s: %w", plan.allocID, err)
	}

	var alloc coordinator.Allocation
	if plan.fresh {
		alloc, err = o.provisionFreshAllocation(ctx, ops, tenant, plan, runID, fence)
	} else {
		alloc, err = o.reuseAllocation(ctx, ops, tenant, ws, plan.allocID, runID)
	}
	if err != nil {
		return "", "", "", err
	}

	// Materialize the run's frozen skills into the allocation (spec §28.16, progressive loading half-2):
	// each pinned skill's sanitized body lands at <alloc>/.palai/skills/<name>/, a sibling of the repo (so
	// it never enters a changeset), readable on-demand via the FileTool. Both the fresh-clone and the
	// reuse path run this, so every run refreshes its own skills; a skill-less run materializes nothing.
	if err := o.store.MaterializeRunSkills(ctx, tenant, runID, func(rel string, body []byte) error {
		_, werr := ops.Write(ctx, rel, body)
		return werr
	}); err != nil {
		return "", "", "", fmt.Errorf("materialize run skills: %w", err)
	}

	// The single writer lease the root run holds for the whole run (spec §29.8), released at attempt end.
	// A crash can leave a dangling ACTIVE lease on the reused allocation; acquireWriterLease reclaims it
	// only on PROOF the holder is dead (E09 T10 devir), never a blind TTL, and never steals a live one.
	leaseID, err = o.acquireWriterLease(ctx, tenant, alloc.ID, runID, jobID)
	if err != nil {
		return "", "", "", err
	}
	// Drive ready→leased. Any failure would LEAK the just-acquired lease (no TTL until E10 recovery, so
	// the session bricks forever — blocker 1), because the caller's defer release is not armed until this
	// returns a leaseID; release it here before surfacing the error.
	//
	// AN ILLEGAL TRANSITION IS TOLERATED ONLY FROM `leased`, AND THE NARROWNESS IS THE POINT — inherited
	// from wip/workspace-idle-release, whose single most valuable line this is. It was once tolerated
	// from ANY state, which was correct while `leased` (a prior attempt whose state-release lost the race
	// to a crash) was the only state this line could be reached in, and the comment justified exactly
	// that one case while the code swallowed every case. An idle releaser adds a second: it claims a
	// workspace into `snapshotting` and then archives and DELETES the directory. A run that acquired its
	// physical lease just before that claim landed would, under a blanket tolerance, sail past this line
	// and be handed a host path whose bytes are being removed underneath it. The claim is atomic and this
	// is where the loser finds out.
	if err := o.spine.AdvanceWorkspace(ctx, tenant, ws.WorkspaceID, statemachines.WorkspaceCmdLease); err != nil {
		// Release under the run's tenant scope, not a bare background context: migration 000029's
		// policies gate this write too, so an unscoped release would affect zero rows and LEAK the
		// lease it means to reclaim.
		scoped := storage.WithTenant(context.Background(), tenant.Project)
		if !errors.Is(err, statemachines.ErrInvalidState) {
			_ = o.spine.ReleaseWriterLease(scoped, leaseID)
			return "", "", "", err
		}
		state, serr := o.spine.WorkspaceLifecycleState(ctx, tenant, ws.WorkspaceID)
		if serr != nil {
			_ = o.spine.ReleaseWriterLease(scoped, leaseID)
			return "", "", "", serr
		}
		if state != string(statemachines.WorkspaceLeased) {
			_ = o.spine.ReleaseWriterLease(scoped, leaseID)
			return "", "", "", fmt.Errorf("%w: workspace %s became %s while this attempt was claiming it",
				coordinator.ErrWriterLeaseHeld, ws.WorkspaceID, state)
		}
		// Already `leased`: the physical lease we just acquired is the authority, as before.
	}
	return alloc.HostPath, leaseID, ws.WorkspaceID, nil
}

// workspaceOpsFor derives the filesystem ONE attempt's workspace tools act on, from the connection
// that attempt holds. It is shellFor one layer over and the two must agree: a command that runs on a
// machine and a file write that lands on the control plane would edit two different trees while
// reporting one.
//
// A CHANNEL THAT REACHES A MACHINE YIELDS THAT MACHINE'S DISK. Everything else yields this host's,
// and that is NOT the fallback shellFor refuses to have. The difference is what is at stake: a shell
// command is model-chosen argv, so running it beside the control plane is arbitrary execution on the
// wrong host and there is no honest default; a workspace operation acts on an allocation THIS PROCESS
// ITSELF realized, because a channel that cannot reach a machine is a channel whose provisioning also
// happened here (realizeRootWorkspace uses the same ops for both). The bytes and the operation stay
// together either way, which is the property that matters.
func workspaceOpsFor(ch EngineChannel, hostPath string) toolbroker.WorkspaceOps {
	if conn, ok := ch.(WorkspaceConn); ok {
		return NewRemoteWorkspace(conn, hostPath)
	}
	return tools.LocalWorkspace(hostPath)
}

// acquireWriterLease takes the root run's single writer lease, reclaiming a stale one a crash left
// behind (E09 T10 devir, spec §29.8). A hard worker kill can leave a still-`active` lease with no TTL,
// which today bricks every later attempt with ErrWriterLeaseHeld. On that conflict this proves the
// holder run is no longer live — RunHasLiveResponseJob(excludeJobID=this attempt's own job) is false —
// then releases the dead lease and re-acquires. The proof is fence/attempt liveness, NOT a blind TTL:
// a LIVE holder's lease is NEVER stolen (the conflict is surfaced unchanged, so a genuine second writer
// still loses). excludeJobID is this attempt's own claimed job, so reclaiming our OWN crashed lease
// (the same run, a new attempt) does not read our current job as the live holder.
func (o *Orchestrator) acquireWriterLease(ctx context.Context, tenant coordinator.Tenant, allocationID, runID, excludeJobID string) (string, error) {
	leaseID := "lease_" + randHex16()
	err := o.spine.AcquireWriterLease(ctx, leaseID, allocationID, runID)
	if !errors.Is(err, coordinator.ErrWriterLeaseHeld) {
		return leaseID, err // acquired, or a non-conflict error (e.g. ErrStaleAllocation) — never a reclaim
	}

	holder, found, herr := o.spine.WorkspaceLeaseHolder(ctx, allocationID)
	if herr != nil {
		return "", herr
	}
	if found {
		live, lerr := o.spine.RunHasLiveResponseJob(ctx, tenant, holder.RunID, excludeJobID)
		if lerr != nil {
			return "", lerr
		}
		if live {
			return "", coordinator.ErrWriterLeaseHeld // a LIVE writer holds it — never steal (single-writer)
		}
		// The holder is dead — a crash left this lease dangling. Release it, then re-acquire.
		if rerr := o.spine.ReleaseWriterLease(ctx, holder.LeaseID); rerr != nil {
			return "", rerr
		}
	}
	retryID := "lease_" + randHex16()
	if err := o.spine.AcquireWriterLease(ctx, retryID, allocationID, runID); err != nil {
		return "", err
	}
	return retryID, nil
}

// provisionFreshAllocation mints the first physical allocation for a workspace: the §29.9 layout, then
// the deterministic clone @ the requested ref (the preparation receipt is the model-independent
// provenance). It drives requested→provisioning→preparing→ready around the clone.
//
// THE DIRECTORY AND THE CLONE BOTH HAPPEN THROUGH `ops`, WHICH IS THE MACHINE (A.3 T5). This function
// used to call workspace.Prepare and repositories.Prepare directly — two writes into the control
// plane's own filesystem — and a run placed on another Mac then found an empty path. The layout is now
// laid down by the runner as it admits the lease (packages/runner/serve.go), and the clone runs on the
// machine, so nothing here touches a disk.
func (o *Orchestrator) provisionFreshAllocation(ctx context.Context, ops toolbroker.WorkspaceOps, tenant coordinator.Tenant, plan workspacePlan, runID string, fence uint64) (coordinator.Allocation, error) {
	ws, allocID, dir := plan.ws, plan.allocID, plan.hostPath
	// The uid this allocation's tools run under, created WITH the allocation. It is acquired before the
	// clone rather than after, because the clone writes into the directory the account must own — an
	// account minted afterwards would inherit a tree written by somebody else.
	//
	// HONEST CEILING: the account is minted on the CONTROL PLANE's host, and after this task the tree it
	// must own is on the MACHINE's. On the one configuration where both are true — a native Mac serving
	// its own runs — this is unchanged and correct. On a split deploy it names a uid that does not own
	// anything, and moving it is the same shape as the rest of this task (a verb the machine performs)
	// rather than a hole in it; it is reported, not worked around.
	if o.sessionAccounts != nil {
		account, err := o.sessionAccounts.Acquire(ctx, plan.sessionID)
		if err != nil {
			return coordinator.Allocation{}, fmt.Errorf("provision %s: %w", allocID, err)
		}
		log.Printf("workspace %s: session %s runs as %s", ws.WorkspaceID, plan.sessionID, account)
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
	// sees (spec §30.2-30.3) — minted here, spent on the machine, revoked here on every return path.
	if err := o.cloneOnMachine(ctx, ops, tenant, plan, runID, fence); err != nil {
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
func (o *Orchestrator) reuseAllocation(ctx context.Context, ops toolbroker.WorkspaceOps, tenant coordinator.Tenant, ws coordinator.SessionWorkspace, allocID, runID string) (coordinator.Allocation, error) {
	alloc, err := o.spine.CurrentAllocation(ctx, ws.WorkspaceID)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	// The allocation the pre-dial plan named is the one being reused; a different one here means the
	// workspace moved between the plan and the dial, and reading a head from a tree this attempt was not
	// planned against would record a base commit for a repository it never saw.
	if alloc.ID != allocID {
		return coordinator.Allocation{}, fmt.Errorf("workspace %s changed allocation between planning (%s) and the dial (%s)",
			ws.WorkspaceID, allocID, alloc.ID)
	}
	// A pause/resume re-enters this for the SAME run: recording a second receipt at the (advanced) head
	// would make RunBaseCommit pick the newest and the changeset miss the pre-pause commits (REP-005). A
	// run keeps the ONE base it started from, so skip when this run already recorded a receipt.
	if _, found, err := o.spine.GetPreparationReceipt(ctx, ws.BindingID, runID); err != nil {
		return coordinator.Allocation{}, err
	} else if found {
		return alloc, nil
	}
	// The head is read WHERE THE REPOSITORY IS. A reused allocation whose machine is not the one this
	// attempt dialed has no repository to read and this fails loudly — see the workspace-affinity note
	// in the A.3 T5 report; a silent empty head would record a base commit of "" and make the run's
	// whole changeset wrong.
	head, tree, err := ops.Head(ctx)
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
	// A fresh context so a canceled/teardown ctx cannot skip the release, but still tenant-scoped: the
	// release + workspace transition are gated by migration 000029's policies, so an unscoped write
	// would silently no-op and leak the lease.
	ctx := storage.WithTenant(context.Background(), tenant.Project)
	if err := o.spine.ReleaseWriterLease(ctx, leaseID); err != nil {
		log.Printf("release writer lease %s: %v", leaseID, err)
	}
	if err := o.spine.AdvanceWorkspace(ctx, tenant, workspaceID, statemachines.WorkspaceCmdRelease); err != nil {
		log.Printf("release workspace %s: %v", workspaceID, err)
	}
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
