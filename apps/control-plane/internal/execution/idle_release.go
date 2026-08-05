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
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

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

// strandedSettleBatch bounds the recovery pass. It is generous next to idleReleaseBatch because the work is
// not comparable: a release archives a tree and deletes a directory, a recovery is one small transaction.
// A hold skipped this pass is a hold next pass.
const strandedSettleBatch = 64

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
// the pass was not clean; a repeat offender (e.g. an allocation over MaxSnapshotArchiveBytes) logs and is
// retried every tick, so a SWEEP-LEVEL summary is what tells an operator "2 stuck" apart from "2 of 32
// stuck" — the per-candidate lines above name them but never say how many candidates there were.
func (r *IdleReleaser) Sweep(ctx context.Context) (released int, err error) {
	if r.snapshots == nil {
		return 0, errors.New("idle release refused: no snapshot sink, so a release would delete unarchived work")
	}
	candidates, lerr := r.spine.IdleWorkspacesForRelease(ctx, r.ttl, idleReleaseBatch)
	if lerr != nil {
		return 0, lerr
	}
	var failed int
	for _, c := range candidates {
		switch done, rerr := r.release(ctx, c); {
		case rerr != nil:
			failed++
			log.Printf("idle release of workspace %s: %v", c.WorkspaceID, rerr)
			if err == nil {
				err = rerr
			}
		case done:
			released++
		}
	}
	if failed > 0 {
		log.Printf("idle release sweep: %d of %d candidates failed and stay ready, retried next tick; %d released", failed, len(candidates), released)
	}
	// THE SECOND PHASE IS THE RECOVERY FOR THE FIRST ONE'S ONE-WAY DOOR, and it runs unconditionally —
	// including after a pass that released nothing and after a pass that failed. Its candidates are holds
	// left open by EARLIER ticks (and by earlier processes), so gating it on this pass's outcome would make
	// the recovery reachable only when a release happened to succeed, which is the opposite of when it is
	// needed. Its error joins this pass's rather than replacing it: both are "the sweep was not clean".
	if serr := r.settleStranded(ctx); serr != nil && err == nil {
		err = serr
	}
	return released, err
}

// settleStranded closes the holds whose closer never ran: open occupancies on machines that were handed
// back. It returns the number settled and the first failure, in the shape Sweep's own loop uses.
//
// WHAT IT REPAIRS, AND WHY IT IS A SWEEP RATHER THAN A REORDERING. release() commits to `paused` before it
// settles, so a settle that fails leaves an archived, deleted, handed-back workspace whose hold is still
// open — and open holds are what AcquireLease's ceiling counts. Until this existed there was no route back
// to such a hold at all, so a deployment that declared a capacity would lose a slot per failed settle,
// permanently, and start refusing real work for a reason nothing logged.
//
// THE ORDER IN release() IS NOT REVERSED, AND THAT IS THE CHOICE THIS FUNCTION IS THE CONSEQUENCE OF.
// Settling first would close the hold before the machine is actually handed back, so a pause that then
// failed would leave a machine holding a session's directory while `released_at IS NULL` — the count
// AcquireLease's ceiling reads — says it is free. That trades a billing leak for capacity
// OVER-subscription, on exactly the axis a declared capacity exists to protect. Settling late instead
// over-counts the machine as occupied for at most one tick, which refuses rather than over-admits.
//
// A LATE SETTLE COSTS THE CUSTOMER NOTHING EXTRA, which is what makes "recover on the next tick" an honest
// answer rather than a deferred bug. The reason is ReleaseReasonIdle here as on the timely path, and an
// idle-closed hold bills `last_activity_at - started_at` (the CASE in storage/queries/leases.sql) — a
// column, not the moment of the settle. So a hold closed a tick late bills exactly what it would have
// billed closed on time.
//
// THE HONEST CEILING — A WORKSPACE STUCK IN `snapshotting` IS NOT REACHED. returnClaim runs on a fresh
// context precisely so a dying sweep still gives the claim back, but a hard process death mid-capture
// leaves the workspace in `snapshotting`, which nothing sweeps and whose hold this does not select.
// `snapshotting` is genuinely ambiguous — a release may be capturing right now — so accepting it here would
// race a live release. That workspace is stuck for reasons wider than billing, and it is named rather than
// quietly folded into a predicate that would look total and not be.
func (r *IdleReleaser) settleStranded(ctx context.Context) error {
	stranded, err := r.spine.StrandedOccupancies(ctx, strandedSettleBatch)
	if err != nil {
		return err
	}
	var first error
	for _, held := range stranded {
		tenant := coordinator.Tenant{Project: held.Project}
		// Keyed by the occupancy's OWN id rather than by its session, so this settles the row the read
		// selected. Going back through the session would re-resolve "the open hold" against a database that
		// may have moved since, which is how a recovery closes something it never looked at.
		settled, serr := r.spine.SettleOccupancy(ctx, tenant, held.ID, coordinator.ReleaseReasonIdle)
		if serr != nil {
			log.Printf("settle stranded occupancy %s of session %s: %v", held.ID, held.SessionID, serr)
			if first == nil {
				first = serr
			}
			continue
		}
		if !settled {
			// Somebody else closed it between the read and the write — the release path's own settle landing
			// late, or another process's sweep. Ordinary, and its bill belongs to the call that closed it.
			continue
		}
		log.Printf("settled a stranded occupancy: session %s held machine time on a workspace that was already handed back (occupancy %s)",
			held.SessionID, held.ID)
	}
	return first
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

	removeErr := r.remove(c.HostPath)

	// 6. THE MACHINE IS HANDED BACK, SO THE OCCUPANCY IS OVER AND ITS MINUTES ARE SETTLED (A.4 T4).
	//
	// IT IS PLACED AFTER THE ACCOUNT AND AFTER THE BYTES ON PURPOSE, and the reason is the same one that
	// puts the capture first: a step that can FAIL must not be able to abort a release that already
	// happened. A meter that refused would leave the archive written, the uid handed back and the
	// directory gone, with a workspace whose hold is still open — that is a worse failure than a late row.
	//
	// AND A LATE ROW IS RECOVERABLE BECAUSE settleStranded EXISTS, which is a correction to what this
	// comment said when the ordering shipped. It said the hold "is still there to settle" and treated that
	// as sufficient; being there is not the same as being reachable, and it was not reachable. This line
	// moves the workspace out of every route back to its own hold — the idle candidate query wants `ready`
	// — so until the recovery pass was added, a settle that failed here leaked a machine slot for good.
	//
	// IT RUNS EVEN THOUGH `remove` FAILED, and this is the branch the whole task turns on. The two failure
	// points on this path read alike (`err != nil`) and mean OPPOSITE things:
	//
	//	capture failed  -> returnClaim -> finish_snapshot -> `ready`, released=false, bytes untouched.
	//	                   THE MACHINE IS STILL THIS SESSION'S. The hold continues; billing it here would
	//	                   charge for a hold that has not ended. That branch returns above and never
	//	                   reaches this line, which is the only reason this line needs no test for it.
	//	remove failed   -> the archive EXISTS and the workspace is `paused`. The machine IS handed back —
	//	                   the next allocation is a fresh directory and nothing will be placed on these
	//	                   bytes. The hold ENDED, so it is closed and billed, and what is left over is a
	//	                   leaked directory, not an open occupancy.
	//
	// Collapsing them into one arm would give the wrong answer to one of them, and which one is only
	// visible on a customer's invoice.
	billErr := r.settleOccupancy(ctx, tenant, c.SessionID)

	if removeErr != nil {
		// THE HOST IS NOT QUARANTINED HERE, and the difference from DestroyAllocation is deliberate.
		// There, a directory that could not be removed may be inherited by a later allocation, so the host
		// stops taking tenants (SAN-008). Here the workspace is already paused and its next allocation is
		// a FRESH directory under a new id — nothing will ever be placed on these bytes — so what is left
		// is this tenant's own data in a path nobody will read. Quarantining the machine for that would
		// take a Mac out of service over a leak that cannot cross a boundary. It is reported, and the
		// release stands: the archive is the authority for what resumes.
		return true, errors.Join(
			fmt.Errorf("idle release left %s on disk (workspace %s is paused and restorable): %w", c.HostPath, c.WorkspaceID, removeErr),
			billErr)
	}
	if billErr != nil {
		return true, billErr
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

// settleOccupancy closes the session's open hold on a machine and settles its machine minutes, in the one
// transaction SettleOccupancy runs — so a hold that is closed is a hold that is billed, and a crash cannot
// leave one without the other.
//
// A SESSION WITH NO OPEN HOLD IS NOT AN ERROR, and there are two ordinary ways to be one. This build has
// been releasing machines since T2/T3 while acquisition only lands with T4, so every workspace already
// paused when this ships has no row to close; and a session whose attempts ran on a channel that reaches
// no machine (the deterministic tiers) never opened one. Both are "nothing to bill", which is the honest
// answer rather than a failure.
//
// THE REASON IS ALWAYS 'idle' HERE, and it is the one word the billed interval branches on: this sweep is
// the idle reaper, so its holds bill to `last_activity_at` and the quiet tail between the customer's last
// activity and this sweep is the platform's cost of keeping the machine warm. What is subtracted is that
// COLUMN and never r.ttl — a TTL is a setting so it changes, and a sweep fires late by however long its
// tick is, and both errors are paid by the customer.
func (r *IdleReleaser) settleOccupancy(ctx context.Context, tenant coordinator.Tenant, sessionID string) error {
	if sessionID == "" {
		return nil
	}
	held, found, err := r.spine.OpenOccupancy(ctx, tenant, sessionID)
	if err != nil {
		return fmt.Errorf("read the open occupancy of session %s: %w", sessionID, err)
	}
	if !found {
		return nil
	}
	if _, err := r.spine.SettleOccupancy(ctx, tenant, held.ID, coordinator.ReleaseReasonIdle); err != nil {
		return fmt.Errorf("settle occupancy %s of session %s: %w", held.ID, sessionID, err)
	}
	return nil
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
	// Ops is the filesystem HostPath is a path ON (Faz A.5 T5). Nil is this process's own — the recovery
	// ladder and the deterministic tiers — and an attempt's ops put the restored tree on the machine that
	// took the lease, which is what the "THE MACHINE MAY BE A DIFFERENT ONE" paragraph below now buys.
	Ops toolbroker.WorkspaceOps
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
// ~~HONEST CEILING — THE RESTORE WRITES TO THIS PROCESS'S FILESYSTEM~~ — CLOSED 2026-08-05 (Faz A.5 T5),
// and the shipped sentence is kept rather than deleted because a superseded ceiling has to be checkable.
// It said snapshot.Restore untars locally so the bytes land wherever the control plane is, and that a
// split deploy would restore onto the wrong disk. The restore now goes through `in.Ops` — the machine's
// own ws.restore, which untars AND verifies SAN-005 on the far side — so a caller inside an attempt
// restores onto the machine that took the lease. A caller with no attempt (nil Ops) is unchanged.
//
// THE CAPTURE HALF IS STILL OPEN AND IS NOT THE SAME SHAPE. IdleReleaser.release archives c.HostPath
// with no Ops, because a sweep has no lease and therefore no connection to any machine; and the machine
// stops serving ws.* the moment the engine exits (packages/runner/serve.go cancels the lease context
// when supervisor.Stream returns), so there is no attempt-end hook to hang one off either. The archives
// a machine's allocation CAN produce today are the ones cut while the engine is live — the pause
// boundary (checkpointBeforePause → captureBoundarySnapshot). Anything else needs the machine's
// workspace service to outlive its engine, which is a lease-lifecycle change and not this task's.
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
	//
	// ‼️ THAT FIRST SENTENCE WAS FALSE FROM THE DAY IT WAS WRITTEN UNTIL A.5 T3, AND IT IS WORTH SAYING
	// WHICH BRANCHES IT IS TRUE OF NOW RATHER THAN WRITING "it is true now". Counted:
	//
	//   1. accounts wired, account acquired, tree handed over, executor root — the restored tree's tools
	//      DO run under this uid. This is the branch the sentence always claimed and never had.
	//   2. accounts wired, executor NOT root — nothing runs at all. HandTreeTo below fails at the first
	//      entry and this function returns; if it somehow did not, host.credentialFor refuses every
	//      command rather than running one as the control plane's uid. The sentence is true by vacuity
	//      and the machine says so, which is the point of the refusal.
	//   3. accounts NOT wired (`accounts == nil`) — no uid is named, no tree is handed over, and the
	//      restored tree's tools run as this process. THE SENTENCE DOES NOT APPLY HERE, and this is the
	//      branch every deployment before A.5 is on and the one macagent/admission.go refuses to enrol a
	//      Mac in. It is a declared posture, not a hole: docs/operations/palai-on-a-mac.md.
	//
	// AND ONE ARM STILL RUNS AS THE CONTROL PLANE IN BRANCH 1, deliberately: this function's own restore.
	// snapshots.RestoreTo untars as this process, which is why the hand-over is AFTER it. The bytes are
	// written by the control plane and then given away; they are never written by the session.
	var account SessionAccount
	if accounts != nil {
		acquired, aerr := accounts.Acquire(ctx, in.SessionID)
		if aerr != nil {
			return coordinator.Allocation{}, fmt.Errorf("resume %s: %w", in.WorkspaceID, aerr)
		}
		account = acquired
	}
	// Scoped for the reason RecoverWorkspace's identical call is: AllocateWorkspace reads the tenant off
	// the context, and this context may carry none.
	alloc, err := spine.AllocateWorkspace(storage.ScopeToTenant(ctx, tenant.Project), in.AllocationID, in.WorkspaceID, in.HostPath)
	if err != nil {
		return coordinator.Allocation{}, err
	}
	manifest, err := snapshots.RestoreThrough(ctx, tenant, snapshotID, in.HostPath, in.Ops)
	if err != nil {
		return coordinator.Allocation{}, fmt.Errorf("%w: restore snapshot %s: %v", ErrRecoveryImpossible, snapshotID, err)
	}
	// The restored bytes are given to the session that will run on them, for provisionFreshAllocation's
	// reason and at the same point in the sequence: after the writer, never before it. A snapshot is
	// untarred by THIS process, so every entry it lays down belongs to the control plane until here.
	if account.Bound() {
		if err := HandTreeTo(in.HostPath, account); err != nil {
			return coordinator.Allocation{}, fmt.Errorf("resume %s: %w", in.WorkspaceID, err)
		}
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
