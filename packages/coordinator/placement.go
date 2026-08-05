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

// ErrRunPoolNotRecordable reports that a placement decision matched NO row: the pool is not one this
// tenant can be served by, so nothing was written and the run still carries no pool.
//
// IT IS AN ERROR RATHER THAN AN EMPTY STRING BECAUSE A CALLER CAN IGNORE A STRING. `RecordRunPool` is
// deliberately a no-op for a foreign or missing pool — a config_policy naming a pool that does not
// exist is a typo, and a foreign-key abort would kill every run in that project — but until
// 2026-08-02 it then returned `nil`, and the caller dialled into the pool it had not recorded, PARKED
// the run there, and left `runs.pool_id` NULL. `OldestRunAwaitingCapacity` matches on exactly that
// column, so two runs on a live stack sat `waiting` for thirty-one hours with no machine able to reach
// them and no reaper covering them.
//
// A write that matched nothing must not report success. That sentence is the general form of the
// defect, and fixing it here rather than at the one call site is what stops the next resolver bug from
// producing the same silent NULL.
var ErrRunPoolNotRecordable = errors.New("run pool is not one this tenant can be served by")

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
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var (
		out      Placement
		recorded *string
	)
	if err := s.pool.QueryRow(ctx, storage.Query("RunPlacementInputs"), runID, tenant.Project).
		Scan(&recorded, &out.QueuedAt, &out.DefaultPoolID); err != nil {
		return Placement{}, fmt.Errorf("read run placement for %s: %w", runID, err)
	}
	if recorded != nil {
		out.PoolID = *recorded
	}
	return out, nil
}

// RecordRunPool writes the placement decision, ONCE, and REPORTS THE POOL THE RUN ENDS UP CARRYING —
// "" when none was recorded. A run that already carries a pool is left alone, so a resume can never be
// re-placed; and a pool that is not this tenant's records NOTHING rather than claiming a decision,
// because a project policy naming a pool that does not exist is a typo and a backfilled default would
// be a lie about where the run went.
//
// THE RETURN VALUE IS WHY THIS SIGNATURE CHANGED, and it is not defensive plumbing. Until 2026-08-02
// this returned only an error, so "recorded nothing" was indistinguishable from "recorded" at the one
// call site that decides whether to dial. A run whose tenant owns no pool resolves to the CONSTANT
// `pool_default`, which belongs to the bootstrap tenant; the EXISTS below excludes it; `runs.pool_id`
// stays NULL — and the dial then parked the run anyway. `OldestRunAwaitingCapacity` matches on
// `r.pool_id = $3`, so NULL matched nothing and NO machine joining ANY pool could ever wake it. Two
// runs sat that way for thirty-one hours on a live stack. The caller can only refuse to park a run it
// cannot record if it is TOLD that it was not recorded.
//
// A no-row UPDATE is ambiguous — the pool may be unusable by this tenant, or a concurrent attempt may
// have recorded it first — and the two answers decide opposite things, so the ambiguous case re-reads
// rather than guessing. It costs one extra round trip on a path that runs at most once per run.
func (s *Store) RecordRunPool(ctx context.Context, tenant Tenant, runID, poolID string) (string, error) {
	if poolID == "" {
		return "", nil
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var recorded string
	switch err := s.pool.QueryRow(ctx, storage.Query("RecordRunPool"),
		runID, tenant.Project, poolID).Scan(&recorded); {
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing was written by THIS call, and the two reasons decide opposite things. Ask the row.
		current, rerr := s.RunPlacement(ctx, tenant, runID)
		if rerr != nil {
			return "", rerr
		}
		if current.PoolID == "" {
			// Nothing recorded and nothing already there: the pool is not one this tenant can be served
			// by. REFUSE rather than report success — see ErrRunPoolNotRecordable.
			return "", fmt.Errorf("%w: %s", ErrRunPoolNotRecordable, poolID)
		}
		// Write-once: a concurrent attempt (or this run's earlier one) already placed it. NOT an error —
		// a resume returns to the SAME pool, and turning this into a failure would break every resume.
		return current.PoolID, nil
	case err != nil:
		return "", fmt.Errorf("record run pool for %s: %w", runID, err)
	}
	return recorded, nil
}

// ParkRunForCapacity releases a run that has no machine to run on: running->waiting, the attempt's
// `awaiting_capacity` marker, and the REFUND of the attempt this park consumed, in ONE transaction. The
// atomicity is the whole of it — a run left waiting without the marker is a run no wake can ever find, a
// marker without the transition would wake a run that never parked, and a park whose refund landed
// separately would charge the run for waiting whenever the second write was the one that died.
//
// THE REFUND IS WHAT MAKES WAITING FREE (Faz A.4 T6), and it is the half E24 T4 could not add because
// nothing yet parked on a full machine. ClaimJob raises `attempt_count` when the attempt is claimed; the
// attempt then finds no machine and parks; EnqueueWokenRunJob correctly carries what the run has spent —
// and so the run paid one attempt for having waited. Five waits dead-lettered a run that had never failed
// at anything, on a platform whose whole point is that a Mac may take six to twenty minutes to arrive. A
// park is neither progress nor a failure. jobID is empty for a direct-drive attempt with no claimed job
// (the test tiers), and then nothing was spent and nothing is returned.
//
// It follows E23 T1's choreography and writes NO second parking mechanism, which is a correctness
// decision rather than a saving: two parking paths mean two waking bugs. What it deliberately does NOT
// do is capture a checkpoint. parkRun takes one when a sink is wired because it parks at a
// boundary an engine reached; this parks at the DIAL or at the machine's refusal, before any engine has
// been spoken to, so there is no boundary to capture and nothing that could offer one. Recovery is rung 2
// — the woken attempt replays the committed transcript — which is always available.
func (s *Store) ParkRunForCapacity(ctx context.Context, tenant Tenant, runID, attemptID, jobID string) error {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("begin capacity park: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdWait); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, storage.Query("MarkAttemptAwaitingCapacity"),
		attemptID, tenant.Project); err != nil {
		return fmt.Errorf("mark attempt awaiting capacity: %w", err)
	}
	if jobID != "" {
		if _, err := tx.Exec(ctx, storage.Query("RefundCapacityParkAttempt"),
			jobID, tenant.Project); err != nil {
			return fmt.Errorf("refund the attempt this capacity park consumed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit capacity park: %w", err)
	}
	return nil
}

// capacityParkBatch bounds one sweep pass, so a large backlog of parked runs cannot hold a pooled
// connection open across the whole reconcile tick. The next tick takes the rest.
const capacityParkBatch = 50

// SweepExpiredCapacityParks ends the runs that have been parked for want of a machine longer than the
// operator's TTL, and it is the reaper §T5 asks for — the one that closes T4's `FLT-P7`: a run parked in a
// pool that will never have a machine waits FOREVER, because the only wake fires when a machine connects.
//
// THERE IS NO DEFAULT TTL. ttl <= 0 is a no-op and that is the shipped posture: AWS documents a Mac host
// taking "approximately 6 minutes to 20 minutes" to start, so a default here would be this file guessing
// how long somebody else's fleet takes to arrive. An operator sets PALAI_FLEET_PARK_TTL or nothing expires.
//
// WHAT "THE ANSWER IS LEARNED RATHER THAN DIED OF" MEANS HERE, and this is a correction to §T5. The plan
// says the run is WOKEN and the model LEARNS the no-capacity answer. Measured against the tree, a capacity
// park happens at the DIAL — before any engine process exists, with no tool call in flight and no model in
// the loop — so there is nothing to hand an answer to, and a plain wake would re-dial into the same empty
// pool and park again on a loop. What CAN learn is the CALLER: the run reaches a terminal `timed_out`
// with a Response body that says a machine never came, which is exactly the shape §20.12's queue deadline
// already uses for a run that waited too long to start (TimeoutQueuedIfExpired). So this reuses that
// choreography rather than inventing a second one, and it reuses the EXISTING `run.timed_out.v1` event and
// the state machine's existing (waiting -> timed_out) edge: no new event type, no migration, nothing added
// to the contract.
//
// Each run settles in its OWN transaction, tenant-re-scoped, exactly as SweepDeadLetteredRuns does: one
// wedged run must not hold the pass, and a sweep that spans tenants to FIND rows still writes under each
// row's own scope so RLS applies to it as it would to a request.
func (s *Store) SweepExpiredCapacityParks(ctx context.Context, ttl time.Duration, projection []byte) (int, error) {
	if ttl <= 0 {
		return 0, nil
	}
	rows, err := s.pool.Query(storage.WithSystemScope(ctx), storage.Query("ExpiredCapacityParks"),
		ttl.Seconds(), capacityParkBatch)
	if err != nil {
		return 0, fmt.Errorf("select expired capacity parks: %w", err)
	}
	type parked struct {
		tenant            Tenant
		runID, responseID string
	}
	var expired []parked
	for rows.Next() {
		var p parked
		if err := rows.Scan(&p.tenant.Project, &p.runID, &p.responseID); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan expired capacity park: %w", err)
		}
		expired = append(expired, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read expired capacity parks: %w", err)
	}

	swept := 0
	for _, p := range expired {
		ended, err := s.timeOutOneCapacityPark(ctx, p.tenant, p.runID, p.responseID, projection)
		if err != nil {
			return swept, err
		}
		if ended {
			swept++
		}
	}
	return swept, nil
}

// timeOutOneCapacityPark drives one parked run to `timed_out` and finalizes its Response in the SAME
// transaction, so a restart reads a coherent terminal — the event and the body land together. A run a
// machine's arrival woke between the scan and this line is not `waiting` any more and is left alone, which
// is what makes the sweep single-winner against the wake: the pass reports what it MOVED, not what it read.
func (s *Store) timeOutOneCapacityPark(ctx context.Context, tenant Tenant, runID, responseID string, projection []byte) (bool, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin capacity-park timeout: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	switch _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdTimeout); {
	case errors.Is(err, ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
		return false, nil // already terminal, or woken and running again, under the run lock
	case err != nil:
		return false, err
	}
	// UpdateResponse excludes terminal states in its WHERE, so a racing terminal write still wins once.
	if _, err := tx.Exec(ctx, storage.Query("UpdateResponse"),
		responseID, tenant.Project, string(statemachines.RunTimedOut), projection); err != nil {
		return false, fmt.Errorf("finalize timed-out capacity park: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit capacity-park timeout: %w", err)
	}
	return true, nil
}

// WakeRunAwaitingCapacity re-enters the OLDEST run parked on a pool for want of a machine and enqueues
// its response.run job in the SAME transaction, so the job becomes claimable only once the wake is
// durable: nothing dispatches before commit. It reports the run it woke, or "" when the pool had none.
//
// TWO EVENTS REACH IT, AND UNTIL Faz A.4 T6 THERE WAS ONLY ONE. A machine JOINING the pool calls it
// through the gateway's handleConnect; a HOLD ENDING calls it through SettleOccupancy. The second is the
// one a ceiling needs: a fleet whose machines are all connected and all full gains no members, so the
// first event never fires and a run parked for want of a slot would wait for a reconnect that says
// nothing about whether a slot exists.
//
// IT WAKES ONE RUN PER CALL, AND IT DOES NOT RESERVE THE MACHINE THAT CALLED IT. One arrival, or one
// settled hold, re-enters one run; the woken run then dials like any other and may lose that machine to a
// different run, in which case it parks again. That race is benign and the alternative is worse:
// reserving would be a second, parallel notion of "assigned to" living next to the durable one the job
// queue already is. What makes it benign is measurable rather than asserted — a park REFUNDS the attempt
// it consumed (RefundCapacityParkAttempt), so a wake that ends in another park costs the run nothing.
//
// FOR UPDATE SKIP LOCKED with a TOTAL order (created_at, id) — not LIMIT 1 on its own, which has
// decided a security outcome in this tree twice. SKIP LOCKED is what makes two machines arriving at
// once wake two DIFFERENT runs instead of contending for one.
//
// ponytail: the predicate has no index and this task cannot add one (E24's only migration is T1's), so
// it is a filtered scan of `runs` once per runner connect and once per settled hold — cheap at self-host
// scale, not at fleet scale. The upgrade path is a partial index and is named in the statement's own
// comment.
func (s *Store) WakeRunAwaitingCapacity(ctx context.Context, tenant Tenant, poolID string) (string, error) {
	if poolID == "" {
		return "", nil
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", fmt.Errorf("begin capacity wake: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	runID, err := wakeRunAwaitingCapacityTx(ctx, tx, tenant, poolID)
	if err != nil || runID == "" {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit capacity wake: %w", err)
	}
	return runID, nil
}

// wakeRunAwaitingCapacityTx is the wake's body, taken out of the method so BOTH events that announce
// capacity run the identical statements against their own transaction. A second, hand-copied wake beside
// this one is how the two would come to disagree about which runs are candidates — which in this file's
// history is not a hypothetical: the park wrote two of the three facts a wake needs and two runs sat
// unreachable for thirty-one hours.
//
// It returns "" with a nil error when the pool had nothing parked, which is the ordinary outcome for both
// callers and never an error for either.
func wakeRunAwaitingCapacityTx(ctx context.Context, tx pgx.Tx, tenant Tenant, poolID string) (string, error) {
	var runID string
	switch err := tx.QueryRow(ctx, storage.Query("OldestRunAwaitingCapacity"),
		tenant.Project, poolID).Scan(&runID); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", nil // capacity appeared on the pool and nothing was waiting on it
	case err != nil:
		return "", fmt.Errorf("select run awaiting capacity: %w", err)
	}
	// The transition + enqueue pair: waiting -> running under the run lock (single-winner, so two
	// announcements cannot re-enter one run) and the job that makes a worker open a fresh attempt.
	if _, err := applyRunTransitionTx(ctx, tx, tenant, runID, statemachines.RunCmdResume); err != nil {
		return "", err
	}
	jobID, err := newJobID()
	if err != nil {
		return "", err
	}
	// The woken job carries the budget the run already spent (EnqueueWokenRunJob). A plain EnqueueJob
	// starts at attempt_count 0, which made every park→wake cycle a fresh five-attempt ladder — so a run
	// failing for a reason that has nothing to do with capacity was retried without bound and reached a
	// terminal only by coincidence. A park is not progress; it is also not a failure, which is the half
	// RefundCapacityParkAttempt adds at the park itself.
	if _, err := tx.Exec(ctx, storage.Query("EnqueueWokenRunJob"),
		jobID, tenant.Project, []byte(fmt.Sprintf(`{"run_id":%q}`, runID)), runID); err != nil {
		return "", fmt.Errorf("enqueue capacity wake job: %w", err)
	}
	return runID, nil
}
