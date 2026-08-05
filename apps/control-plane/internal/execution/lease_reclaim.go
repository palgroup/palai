package execution

import (
	"context"
	"errors"
	"log"
	"time"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// DefaultAbandonedLeaseGrace is how long after a run reaches terminal the sweep waits before it will
// touch that run's writer lease. It exists for ONE window and it is a short one: UpdateRunState commits
// the terminal transition before ExecuteAttempt's deferred releaseWorkspace has run, so for the length of
// an attempt's unwind the run reads terminal while the process that owns the lease is still finishing
// with the directory. Two minutes is far longer than that unwind and far shorter than the interval over
// which a held machine matters.
//
// It is NOT the idle TTL and must not be confused with it. The idle TTL asks "has this thread gone quiet
// long enough to take its machine away", a question about a person; this asks "has the process that held
// this lease finished exiting", a question about a goroutine. Making the second one long would leave a
// machine held for no reason; making the first one short would take a workspace away from someone still
// reading their diff.
const DefaultAbandonedLeaseGrace = 2 * time.Minute

// abandonedLeaseBatch bounds one pass. Each reclaim is two small writes, so this is generous next to
// idleReleaseBatch; a candidate skipped this pass is a candidate on the next one.
const abandonedLeaseBatch = 64

// LeaseReclaimer returns a workspace whose holder run reached terminal from `leased` back to `ready`.
//
// WHY IT HAS TO EXIST, WHICH IS NOT OBVIOUS BECAUSE A RECLAIM ALREADY EXISTED. acquireWriterLease has
// reclaimed dead leases since E09 T10, and it is correct — but it runs from exactly one place, on a lease
// CONFLICT, which means it fires only when a SECOND attempt arrives on the same allocation. A thread whose
// last run ended and which nobody writes to again never produces that second attempt. Its lease stays
// active, its workspace stays `leased`, and IdleWorkspacesForRelease selects `state = 'ready'` — so the
// idle releaser, the one thing that hands a machine back, can never see it. Nothing is broken and nothing
// is logged; the directory and the uid slot are simply held until somebody types into that thread again.
//
// Measured on the running stack 2026-08-05, after the release defer was repaired: 19 active leases and 19
// `leased` workspaces, ZERO of them with a live run — 16 completed, 2 canceled, 1 failed — holding 197MB
// across 18 allocation directories, the oldest abandoned for 9h43m.
//
// THIS IS STAGE ONE OF TWO AND THAT SPLIT IS THE SAFETY ARGUMENT. The obvious repair is to widen the idle
// sweep to accept `leased`, and it is the wrong one: that sweep ARCHIVES AND DELETES a directory, so
// pointing it at a live lease would destroy work a running attempt is in the middle of. This stage does
// the cheap, reversible half — release the lease, leased→ready, not one byte touched — and then the
// EXISTING idle sweep picks the workspace up on its own terms (ready, no active lease, idle for the TTL)
// and does the half that deletes. workspaces.sql already says the split out loud: the idle candidate query
// documents skipping a dangling lease as "the stuck-lease reclaim's business, not this sweep's". That
// reclaim is this.
//
// It is the retention Reaper's and the IdleReleaser's sibling by construction: a durable maintenance job
// running one bounded, tenant-safe pass per tick, whose errors are logged and retried on the next tick
// rather than being fatal.
type LeaseReclaimer struct {
	spine *coordinator.Store
	grace time.Duration
}

// NewLeaseReclaimer binds the durable spine and the post-terminal grace.
func NewLeaseReclaimer(spine *coordinator.Store, grace time.Duration) *LeaseReclaimer {
	return &LeaseReclaimer{spine: spine, grace: grace}
}

// Sweep runs one reclaim pass and returns the number of workspaces returned to ready. A failure on one
// candidate is logged and the pass continues: one workspace whose transition loses a race must not keep
// every other machine held. The first error is returned so a caller can surface that the pass was not
// clean.
func (r *LeaseReclaimer) Sweep(ctx context.Context) (reclaimed int, err error) {
	candidates, lerr := r.spine.AbandonedWriterLeases(ctx, r.grace, abandonedLeaseBatch)
	if lerr != nil {
		return 0, lerr
	}
	for _, c := range candidates {
		if rerr := r.reclaim(ctx, c); rerr != nil {
			log.Printf("reclaim abandoned lease %s on workspace %s: %v", c.LeaseID, c.WorkspaceID, rerr)
			if err == nil {
				err = rerr
			}
			continue
		}
		reclaimed++
		log.Printf("reclaimed workspace %s from a lease abandoned by terminal run %s: leased -> ready", c.WorkspaceID, c.RunID)
	}
	return reclaimed, err
}

// reclaim performs the two writes releaseWorkspace performs, in the same order and for the same reason.
//
// THE LEASE GOES FIRST. A workspace that reached `ready` while an active lease still names it is the one
// state the idle sweep explicitly refuses to touch (its candidate query requires NO active lease), so an
// interruption between these two writes leaves the workspace exactly where it started rather than in a
// state nothing sweeps. Reversed, the same interruption would produce ready-with-a-lease, which the next
// pass could not see either — the workspace would no longer be `leased`, so it would fall out of this
// sweep's candidate set as well, and be stuck for good.
//
// AN ILLEGAL TRANSITION IS NOT AN ERROR. Between the read and this write a new attempt may have taken the
// workspace, or ExecuteAttempt's own deferred release may have arrived; either way the workspace is no
// longer `leased`, the state machine refuses, and the right outcome is to move on having done no harm.
func (r *LeaseReclaimer) reclaim(ctx context.Context, c coordinator.AbandonedLease) error {
	tenant := coordinator.Tenant{Project: c.Project}
	// THE CANDIDATE READ IS SYSTEM-SCOPED AND EVERY WRITE BELOW IS NOT, which is why the scope is
	// re-applied here from the row's OWN project rather than inherited. ReleaseWriterLease takes no
	// tenant argument — it is keyed by the opaque lease id, so the CONTEXT is the only thing carrying
	// the scope migration 000029's policies check, exactly as releaseWorkspace passes it.
	ctx = storage.WithTenant(ctx, c.Project)
	if err := r.spine.ReleaseWriterLease(ctx, c.LeaseID); err != nil {
		return err
	}
	if err := r.spine.AdvanceWorkspace(ctx, tenant, c.WorkspaceID, statemachines.WorkspaceCmdRelease); err != nil {
		if errors.Is(err, statemachines.ErrInvalidState) {
			return nil // somebody else got there first; the lease is released either way
		}
		return err
	}
	return nil
}

// Run drives one sweep per interval until ctx ends, in the shape the Supervisor restarts.
func (r *LeaseReclaimer) Run(ctx context.Context, interval time.Duration) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if reclaimed, err := r.Sweep(ctx); err != nil {
				log.Printf("abandoned lease reclaim sweep failed after reclaiming %d: %v", reclaimed, err)
			}
		}
	}
}
