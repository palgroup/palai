package coordinator

// PLACEMENT AND THE CAPACITY PARK (E24 T4).
//
// Two durable facts and two durable transitions:
//
//   - WHERE a run was placed (`runs.pool_id`, migration 000045 R5), so the decision is auditable and a
//     resume returns to the SAME pool. A run that woke in a different posture after a kill is a run
//     that cannot find its workspace.
//   - THE PARK: a run whose pool holds no machine goes running->waiting and its ATTEMPT is marked
//     `awaiting_capacity`, atomically. Before this, such a run spent a 20s dial budget five times and
//     dead-lettered in about two and a half minutes — while AWS documents a Mac host taking six to
//     twenty minutes to start (§3.6 D12, §3.5 P10). So "bring a Mac up when load arrives" was not an
//     economic choice, it was an unreachable behaviour.
//
// WHY THE MARKER IS ON THE ATTEMPT AND NOT ON THE RUN. `waiting` is not one condition, it is four: a
// human's pause, a gated tool call, a detached child, and now no capacity — and each has its own waker
// with its own predicate (WakeDetachedParent counts children; the approval wake reads its approvals
// row). A capacity wake fires on every machine that connects, which for a runner is after every single
// lease, so waking on `state = 'waiting'` alone would resume paused runs against their user's decision
// and re-drive approval-parked runs in a loop. The predicate has to be POSITIVE and it has to identify
// THIS reason.
//
// `attempts.state` is that marker, and it costs no migration — which matters because E24 owns exactly
// one and T1 owns it. The column has only ever held 'assigned' (the insert default) and 'preempted' (a
// supersede), nothing reads it for a decision, and its own note in responses.sql says attempt-lifecycle
// terminal writes are still unbuilt. It is also the honest home for the fact: it is the ATTEMPT that
// found no machine. The marker is cleared by the supersede the NEXT attempt already performs, so there
// is no second write to forget and no window where a woken run still looks parked.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	statemachines "github.com/palgroup/palai/packages/state-machines"
	"github.com/palgroup/palai/storage"
)

// AttemptAwaitingCapacity is the attempts.state value that marks the one waiting reason a machine's
// arrival may wake. Exported so a proof can assert on the durable marker rather than on a behaviour
// that happens to follow from it.
const AttemptAwaitingCapacity = "awaiting_capacity"

// Placement is what a placement decision reads about a run.
type Placement struct {
	// PoolID is runs.pool_id — the pool this run was ALREADY placed in, empty when no decision has been
	// recorded (which is every run in every deployment before this task).
	PoolID string
	// QueuedAt is the RUN's created_at, which is what orders a pool's waiting attempts. The distinction
	// from the attempt's arrival is the whole point: a run bounced by a cordon, a retry or a resume
	// re-dials with its ORIGINAL timestamp and keeps its place instead of going to the back of the line.
	QueuedAt time.Time
	// DefaultPoolID is THIS TENANT's own pool named 'default' — the one identity.Store.provision seeds
	// with every organization — and it is here because `fleet.DefaultPoolID` is a CONSTANT. The constant
	// is the bootstrap tenant's pool id, so every other tenant that had configured nothing resolved to a
	// pool belonging to somebody else. Empty when the tenant has no default pool, which leaves the
	// constant as the last resort exactly as before.
	DefaultPoolID string
}

// RunPlacement reads a run's placement inputs in one round trip. Tenant-scoped: unlike RunContext this
// is not the read that ESTABLISHES the tenant — ExecuteAttempt has already resolved one and re-scoped
// the context to it — so it publishes it and RLS confines the read.
func (s *Store) RunPlacement(ctx context.Context, tenant Tenant, runID string) (Placement, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	var (
		out      Placement
		recorded *string
	)
	if err := s.pool.QueryRow(ctx, storage.Query("RunPlacementInputs"), runID, tenant.Organization, tenant.Project).
		Scan(&recorded, &out.QueuedAt, &out.DefaultPoolID); err != nil {
		return Placement{}, fmt.Errorf("read run placement for %s: %w", runID, err)
	}
	if recorded != nil {
		out.PoolID = *recorded
	}
	return out, nil
}

// RecordRunPool writes the placement decision, ONCE. A run that already carries a pool is left alone,
// so a resume can never be re-placed — and a pool that is not this tenant's records NOTHING rather than
// claiming a decision, because a project policy naming a pool that does not exist is a typo and a
// backfilled default would be a lie about where the run went. Such a run parks and stays parked, which
// is the honest outcome for a pool that will never have a machine (the reaper is T5's).
func (s *Store) RecordRunPool(ctx context.Context, tenant Tenant, runID, poolID string) error {
	if poolID == "" {
		return nil
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	if _, err := s.pool.Exec(ctx, storage.Query("RecordRunPool"),
		runID, tenant.Organization, tenant.Project, poolID); err != nil {
		return fmt.Errorf("record run pool for %s: %w", runID, err)
	}
	return nil
}

// ParkRunForCapacity releases a run whose pool holds no machine: running->waiting plus the attempt's
// `awaiting_capacity` marker, in ONE transaction. The atomicity is the whole of it — a run left waiting
// without the marker is a run no wake can ever find, and a marker without the transition would wake a
// run that never parked.
//
// It follows E23 T1's choreography and writes NO second parking mechanism, which is a correctness
// decision rather than a saving: two parking paths mean two waking bugs. What it deliberately does NOT
// do is capture a checkpoint. parkForApproval takes one when a sink is wired because it parks at a
// boundary an engine reached; this parks at the DIAL, before any engine exists, so there is no boundary
// to capture and nothing that could offer one. Recovery is rung 2 — the woken attempt replays the
// committed transcript — which is always available.
func (s *Store) ParkRunForCapacity(ctx context.Context, tenant Tenant, runID, attemptID string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin capacity park: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdWait); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("MarkAttemptAwaitingCapacity"),
		attemptID, tenant.Organization, tenant.Project); err != nil {
		return fmt.Errorf("mark attempt awaiting capacity: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit capacity park: %w", err)
	}
	return nil
}

// WakeRunAwaitingCapacity re-enters the OLDEST run parked on a pool for want of a machine and enqueues
// its response.run job in the SAME transaction, so the job becomes claimable only once the wake is
// durable: nothing dispatches before commit. It reports the run it woke, or "" when the pool had none.
//
// IT WAKES ONE RUN PER CALL, AND IT DOES NOT RESERVE THE MACHINE THAT CALLED IT. The caller is a
// machine joining the pool, so one machine's arrival re-enters one run; the woken run then dials like
// any other and may lose that machine to a different run, in which case it parks again. That race is
// benign and the alternative is worse: reserving would be a second, parallel notion of "assigned to"
// living next to the durable one the job queue already is.
//
// FOR UPDATE SKIP LOCKED with a TOTAL order (created_at, id) — not LIMIT 1 on its own, which has
// decided a security outcome in this tree twice. SKIP LOCKED is what makes two machines arriving at
// once wake two DIFFERENT runs instead of contending for one.
//
// ponytail: the predicate has no index and this task cannot add one (E24's only migration is T1's), so
// it is a filtered scan of `runs` once per runner connect — cheap at self-host scale, not at fleet
// scale. The upgrade path is a partial index and is named in the statement's own comment.
func (s *Store) WakeRunAwaitingCapacity(ctx context.Context, tenant Tenant, poolID string) (string, error) {
	if poolID == "" {
		return "", nil
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Organization, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", fmt.Errorf("begin capacity wake: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var runID string
	switch err := tx.QueryRow(ctx, storage.Query("OldestRunAwaitingCapacity"),
		tenant.Organization, tenant.Project, poolID).Scan(&runID); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil // the pool gained a machine and nothing was waiting on it
	case err != nil:
		return "", fmt.Errorf("select run awaiting capacity: %w", err)
	}
	// The wake body is wakeParkedRunTx's, reached through the same transition + enqueue pair: waiting ->
	// running under the run lock (single-winner, so two machines cannot re-enter one run) and the job
	// that makes a worker open a fresh attempt.
	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdResume); err != nil {
		return "", err
	}
	jobID, err := newJobID()
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, storage.Query("EnqueueJob"),
		jobID, tenant.Organization, tenant.Project, "response.run", []byte(fmt.Sprintf(`{"run_id":%q}`, runID))); err != nil {
		return "", fmt.Errorf("enqueue capacity wake job: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit capacity wake: %w", err)
	}
	return runID, nil
}
