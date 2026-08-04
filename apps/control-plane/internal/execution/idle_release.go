package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// DefaultIdleWorkspaceTTL is how long a session's workspace may hold a machine with nothing happening on
// it before the sweep archives it and hands the machine back. Five minutes is the operator-facing number:
// long enough that a human reading a diff and typing a reply keeps their workspace warm, short enough
// that a Mac is not held overnight by a conversation somebody walked away from.
//
// A knob (PALAI_WORKSPACE_IDLE_TTL), not a switch. There is no "off": a deployment that wants a workspace
// held longer says how much longer.
const DefaultIdleWorkspaceTTL = 5 * time.Minute

// idleReleaseBatch bounds one pass. Each release archives a whole allocation and deletes a directory
// tree, so an unbounded pass on a busy machine would be a long tail of IO holding a maintenance
// goroutine. A candidate skipped this pass is a candidate on the next one — its TTL only grows.
const idleReleaseBatch = 32

// IdleReleaser hands a machine back when a session stops using it, WITHOUT ending the session.
//
// THE SESSION IS THE THREAD'S IDENTITY AND IT DOES NOT CLOSE. What closes is the physical claim on a
// machine: the allocation. A workspace idle for the TTL is archived (bytes and .git, so uncommitted work
// survives — SAN-005) and its host directory reclaimed; the logical workspace id, the session, and the
// conversation are untouched. The next message in that thread reaches planRootWorkspace, which finds the
// workspace `paused` and restores it onto a NEW allocation — possibly on a different machine, because the
// bytes live in the object store and nothing about the restore names the machine that captured them.
//
// WHY NOT close_session, WHICH IS THE OTHER THING THAT COULD HAVE BEEN WIRED HERE. Measured 2026-08-05:
// 281 of 292 sessions on this stack were `active`, the oldest for days, and nothing in the tree sends
// close_session at all. But closing them would have been the wrong repair twice over. First, it frees
// almost nothing: applyCloseSessionTx touches sessions, commands, events and session_sequences and not
// one workspace table — the directory and the uid would still leak. Second, `closed` is a lifecycle exit,
// so the next message in that thread would be refused rather than resumed, which is the opposite of what
// an idle thread needs. The occupancy is the allocation; the identity is the session; only the first is
// reclaimable while the conversation is still alive.
//
// It is the retention Reaper's sibling by construction (retention.go): a durable maintenance job on the
// coordinator running one bounded, tenant-safe pass per tick, whose errors are logged and retried on the
// next tick rather than being fatal.
//
// WHY THIS IS NOT DestroyAllocation. That path drives ready→destroying→destroyed, and `destroyed` is
// terminal in the WorkspaceTable — a workspace that reached it can never come back, which is right for a
// workspace nobody will ask for again and wrong for every one of these. The releaser uses the
// pause/restore branch instead, which is the branch that was built for exactly this and had no driver.
type IdleReleaser struct {
	spine     *coordinator.Store
	snapshots *SnapshotSink
	ttl       time.Duration
	// remove reclaims an allocation's host directory. Defaulted to os.RemoveAll; a test injects a failing
	// remover to drive the leaked-bytes path. One function field, not an interface — there is exactly one
	// real implementation, the same call WorkspaceRecovery makes.
	remove func(string) error
	// accounts releases the uid the allocation's tools ran under. Nil means the deployment mints none,
	// which is what every non-macOS deployment does.
	accounts SessionAccounts
}

// NewIdleReleaser binds the durable spine, the snapshot capture path, and the idle TTL.
//
// THE SNAPSHOT SINK IS NOT OPTIONAL, unlike the Reaper's artifact deleter. Releasing a workspace whose
// bytes were never archived is not a weaker release, it is data loss: the directory holding a session's
// uncommitted work would be deleted with nothing to restore from. A composition root with no object store
// must not construct this at all — main.go's startIdleRelease says so and refuses — and a nil sink here
// makes every sweep refuse rather than delete.
func NewIdleReleaser(spine *coordinator.Store, snapshots *SnapshotSink, ttl time.Duration) *IdleReleaser {
	return &IdleReleaser{spine: spine, snapshots: snapshots, ttl: ttl, remove: os.RemoveAll}
}

// WithSessionAccounts wires the macOS session-account release, so an idle workspace hands back its uid
// slot as well as its bytes. Without it a machine runs out of the 99 slots mac-sessions.sh allocates long
// before it runs out of disk, because nothing else on this path frees one.
func (r *IdleReleaser) WithSessionAccounts(a SessionAccounts) *IdleReleaser {
	r.accounts = a
	return r
}

// SetTeardown overrides how an allocation's host directory is reclaimed (default os.RemoveAll). Same seam
// as WorkspaceRecovery.SetTeardown: a remote tier tears down over the wire, and a test drives the failure
// path through it.
func (r *IdleReleaser) SetTeardown(remove func(string) error) { r.remove = remove }

// Sweep runs one idle pass and returns the number of workspaces released. A failure on one candidate is
// logged and the pass continues to the next: one workspace whose archive will not upload must not stop
// every other machine from being handed back. The first error is returned so a caller can surface that
// the pass was not clean.
func (r *IdleReleaser) Sweep(ctx context.Context) (released int, err error) {
	if r.snapshots == nil {
		return 0, errors.New("idle release refused: no snapshot sink, so a release would delete unarchived work")
	}
	candidates, lerr := r.spine.IdleWorkspacesForRelease(ctx, r.ttl, idleReleaseBatch)
	if lerr != nil {
		return 0, lerr
	}
	for _, c := range candidates {
		switch done, rerr := r.release(ctx, c); {
		case rerr != nil:
			log.Printf("idle release of workspace %s: %v", c.WorkspaceID, rerr)
			if err == nil {
				err = rerr
			}
		case done:
			released++
		}
	}
	return released, err
}

// release archives one idle workspace and reclaims its machine. It returns false with no error when the
// candidate stopped being idle between the sweep's read and its claim — the ordinary outcome of losing a
// race, not a failure.
//
// THE ORDER IS THE SAFETY PROPERTY, so it is written here rather than left to be inferred:
//
//  1. CLAIM ready→snapshotting, through the state machine, which locks the row. From snapshotting a run
//     cannot take the workspace (`lease` is legal only from ready), so everything below happens over a
//     tree with no writer. An illegal transition here means a run got there first: give up, having
//     touched nothing.
//  2. RE-ASK for an active lease. The claim and a racing acquireWriterLease do not serialize — one is
//     guarded by the workspaces row, the other by the allocation fence — so a lease acquired in the
//     window between them would be invisible to step 1. Seeing one, give the claim back.
//  3. CAPTURE. Fence-guarded: a capture against a superseded allocation affects zero rows and returns
//     ErrStaleAllocation rather than archiving the wrong tree.
//  4. On any failure in 2-3, RETURN THE CLAIM (finish_snapshot → ready) with the bytes untouched. The
//     workspace is exactly where it started and the next tick may try again.
//  5. Only once an archive EXISTS does the workspace move snapshotting→paused, and only after that are
//     the bytes deleted. The archive is therefore never the thing that is missing when the directory is
//     gone.
func (r *IdleReleaser) release(ctx context.Context, c coordinator.IdleWorkspace) (bool, error) {
	tenant := coordinator.Tenant{Project: c.Project}

	// 1. Claim.
	if err := r.spine.AdvanceWorkspace(ctx, tenant, c.WorkspaceID, statemachines.WorkspaceCmdSnapshot); err != nil {
		if errors.Is(err, statemachines.ErrInvalidState) {
			return false, nil // a run took it between the read and the claim
		}
		return false, err
	}

	// 2. Re-ask for a lease the claim could not have seen.
	held, err := r.spine.WorkspaceHasActiveLease(ctx, tenant, c.WorkspaceID)
	if err != nil {
		return false, errors.Join(err, r.returnClaim(tenant, c.WorkspaceID))
	}
	if held {
		return false, r.returnClaim(tenant, c.WorkspaceID)
	}

	// 3. Capture. Reason "idle" distinguishes this boundary from the "pause" one a run's own pause cuts:
	// both are restorable, but only this one means "the machine was handed back".
	snapshotID := "snap_" + randHex16()
	if _, err := r.snapshots.Capture(ctx, SnapshotCaptureInput{
		SnapshotID:   snapshotID,
		Project:      c.Project,
		WorkspaceID:  c.WorkspaceID,
		AllocationID: c.AllocationID,
		HostPath:     c.HostPath,
		Reason:       "idle",
	}); err != nil {
		return false, errors.Join(fmt.Errorf("capture idle snapshot: %w", err), r.returnClaim(tenant, c.WorkspaceID))
	}

	// 5. The archive exists — commit to the release.
	if err := r.spine.AdvanceWorkspace(ctx, tenant, c.WorkspaceID, statemachines.WorkspaceCmdPause); err != nil {
		return false, err
	}

	// THE ACCOUNT GOES BEFORE THE BYTES, for the reason DestroyAllocation gives: destroying the account
	// kills every process still running as that uid, so the directory removal below is not racing a build
	// that is still writing into it. A failure does not stop the release — the slot is held until an
	// operator reconciles (mac-sessions.sh down), and the machine's disk is the more urgent of the two
	// residues. It is keyed by SESSION, which is the key SessionAccounts is defined on and the key Acquire
	// was called with; an allocation id here would miss the map and destroy nothing while returning nil.
	if r.accounts != nil {
		if err := r.accounts.Release(ctx, c.SessionID); err != nil {
			log.Printf("idle release of workspace %s: session %s account not released, its slot is held until an operator reconciles: %v",
				c.WorkspaceID, c.SessionID, err)
		}
	}

	if err := r.remove(c.HostPath); err != nil {
		// THE HOST IS NOT QUARANTINED HERE, and the difference from DestroyAllocation is deliberate.
		// There, a directory that could not be removed may be inherited by a later allocation, so the host
		// stops taking tenants (SAN-008). Here the workspace is already paused and its next allocation is
		// a FRESH directory under a new id — nothing will ever be placed on these bytes — so what is left
		// is this tenant's own data in a path nobody will read. Quarantining the machine for that would
		// take a Mac out of service over a leak that cannot cross a boundary. It is reported, and the
		// release stands: the archive is the authority for what resumes.
		return true, fmt.Errorf("idle release left %s on disk (workspace %s is paused and restorable): %w", c.HostPath, c.WorkspaceID, err)
	}

	payload, _ := json.Marshal(map[string]any{
		"workspace_id":  c.WorkspaceID,
		"allocation_id": c.AllocationID,
		"snapshot_id":   snapshotID,
		"idle_ttl":      r.ttl.String(),
	})
	if _, err := r.spine.RecordRecoveryEvent(ctx, tenant, c.SessionID, "", eventWorkspacePaused, payload); err != nil {
		return true, fmt.Errorf("journal idle release of workspace %s: %w", c.WorkspaceID, err)
	}
	return true, nil
}

// returnClaim gives a claimed workspace back to ready with its bytes untouched (snapshotting →
// finish_snapshot → ready). It takes no caller context on purpose: the claim must be returned even when
// the sweep's own context is the thing that just died, or a shutdown mid-pass would strand a workspace in
// snapshotting — a state nothing leases and only the resume path's repair would ever move again.
func (r *IdleReleaser) returnClaim(tenant coordinator.Tenant, workspaceID string) error {
	fresh := storage.ScopeToTenant(context.Background(), tenant.Project)
	if err := r.spine.AdvanceWorkspace(fresh, tenant, workspaceID, statemachines.WorkspaceCmdFinishSnapshot); err != nil {
		return fmt.Errorf("return idle claim on workspace %s: %w", workspaceID, err)
	}
	return nil
}

// ResumeInput names the paused workspace to bring back, the thread waking it, and the allocation the
// caller has ALREADY minted for it. The id and the path come from the caller rather than being minted
// here because on this architecture the control plane names an allocation's path BEFORE the dial — the
// lease offer carries it and the machine creates it (planRootWorkspace / packages/runner/serve.go). A
// resume that minted its own would restore into a directory the runner never mounted.
type ResumeInput struct {
	WorkspaceID  string
	SessionID    string
	RunID        string
	ResponseID   string
	AllocationID string
	HostPath     string
	// HostID is the placement identity the quarantine guard consults (the provision root, on this tier).
	HostID string
}

// ResumeReleasedWorkspace brings back a workspace the idle releaser handed a machine back for: it drives
// paused→restoring, records the caller's NEW fenced allocation, restores the latest byte-archived snapshot
// into it (checksum-verified, SAN-005), then restoring→ready and journals workspace.restored.v1.
//
// IT IS THE HALF THAT MAKES THE RELEASE SAFE, and neither may ship without the other. A releaser with no
// resume turns every idle thread into a dead one: planRootWorkspace's `paused` arm refused outright, so
// the next message in that conversation would get an error instead of its files. The release and the
// resume are one change, which is why they share a file.
//
// THE MACHINE MAY BE A DIFFERENT ONE, and nothing here has to know. The logical workspace id is stable,
// the bytes come from the object store, and the fence only ever goes up — so a session captured on one Mac
// resumes on whichever Mac the control plane is provisioning on now, with the previous host's writes
// already fenced out at the DB (SAN-006).
//
// HONEST CEILING — THE RESTORE WRITES TO THIS PROCESS'S FILESYSTEM. snapshot.Restore untars locally, so
// the bytes land wherever the control plane is, exactly as RecoverWorkspace's restore does. On a native
// Mac serving its own runs — the configuration this is being shipped for — the control plane and the
// machine are the same host and this is correct. On a split deploy it would restore onto the wrong disk,
// which is the same shape as the session-account ceiling provisionFreshAllocation already names, and it
// is reported rather than worked around.
//
// A workspace with no restorable snapshot FAILS rather than resuming on an empty tree: a paused workspace
// whose archive is gone has lost its work, and re-cloning the binding would present that loss as a fresh
// start.
func ResumeReleasedWorkspace(
	ctx context.Context,
	spine *coordinator.Store,
	snapshots *SnapshotSink,
	accounts SessionAccounts,
	tenant coordinator.Tenant,
	in ResumeInput,
) (coordinator.Allocation, error) {
	if snapshots == nil {
		return coordinator.Allocation{}, fmt.Errorf("%w: workspace %s is paused and no snapshot sink is wired to restore it",
			ErrRecoveryImpossible, in.WorkspaceID)
	}
	// paused→restoring. An already-`restoring` workspace (a restore that died half-way) tolerates the
	// illegal transition and re-enters; the physical restore below is the authority either way.
	if err := spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdRestore); err != nil && !errors.Is(err, statemachines.ErrInvalidState) {
		return coordinator.Allocation{}, err
	}
	// The same placement guard RecoverWorkspace applies: never restore onto a host whose bytes an earlier
	// teardown could not reclaim (SAN-008).
	if in.HostID != "" {
		quarantined, err := spine.IsHostQuarantined(ctx, in.HostID)
		if err != nil {
			return coordinator.Allocation{}, err
		}
		if quarantined {
			return coordinator.Allocation{}, fmt.Errorf("%w: %s", ErrHostQuarantined, in.HostID)
		}
	}
	snapshotID, found, err := spine.LatestRestorableWorkspaceSnapshot(ctx, tenant, in.WorkspaceID)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	if !found {
		return coordinator.Allocation{}, fmt.Errorf("%w: no byte-archived snapshot for paused workspace %s", ErrRecoveryImpossible, in.WorkspaceID)
	}
	// The uid the restored tree's tools run under, acquired BEFORE the bytes land for the reason
	// provisionFreshAllocation gives: an account minted afterwards inherits a tree written by somebody
	// else. The session's slot was released when the machine was handed back, so this mints a new one.
	if accounts != nil {
		account, aerr := accounts.Acquire(ctx, in.SessionID)
		if aerr != nil {
			return coordinator.Allocation{}, fmt.Errorf("resume %s: %w", in.WorkspaceID, aerr)
		}
		log.Printf("workspace %s: session %s resumes as %s", in.WorkspaceID, in.SessionID, account)
	}
	// Scoped for the reason RecoverWorkspace's identical call is: AllocateWorkspace reads the tenant off
	// the context, and this context may carry none.
	alloc, err := spine.AllocateWorkspace(storage.ScopeToTenant(ctx, tenant.Project), in.AllocationID, in.WorkspaceID, in.HostPath)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	manifest, err := snapshots.RestoreTo(ctx, tenant, snapshotID, in.HostPath)
	if err != nil {
		return coordinator.Allocation{}, fmt.Errorf("%w: restore snapshot %s: %v", ErrRecoveryImpossible, snapshotID, err)
	}
	if err := spine.AdvanceWorkspace(ctx, tenant, in.WorkspaceID, statemachines.WorkspaceCmdMarkReady); err != nil {
		return coordinator.Allocation{}, err
	}
	payload, _ := json.Marshal(map[string]any{
		"workspace_id":      in.WorkspaceID,
		"run_id":            in.RunID,
		"new_allocation_id": alloc.ID,
		"new_fence":         alloc.Fence,
		"snapshot_id":       snapshotID,
		"tree_checksum":     manifest.TreeChecksum,
	})
	if _, err := spine.RecordRecoveryEvent(ctx, tenant, in.SessionID, in.ResponseID, eventWorkspaceRestored, payload); err != nil {
		return coordinator.Allocation{}, err
	}
	return alloc, nil
}

// Run sweeps every interval until ctx is cancelled, exactly as the retention Reaper does. A sweep error is
// logged and non-fatal: a transient database blip must not stop machines being handed back, and the next
// tick retries from the durable state rather than from anything this goroutine remembers.
func (r *IdleReleaser) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if released, err := r.Sweep(ctx); err != nil {
				log.Printf("workspace idle release sweep failed after releasing %d: %v", released, err)
			}
		}
	}
}
