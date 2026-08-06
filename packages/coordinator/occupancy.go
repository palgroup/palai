package coordinator

// MACHINE OCCUPANCY (Faz A.4 T1, migration 000003_lease_occupancy).
//
// An occupancy is the interval in which one session held one machine. `runner_leases` has carried a
// skeleton of it since the baseline and has never had a writer — measured immediately before this file:
//
//	grep -rniE 'INSERT INTO runner_leases' --include='*.go' --include='*.sql' . | grep -v node_modules | wc -l
//	→ 0   (2026-08-04)
//
// This file is that writer. Two things are billed and until now only one of them was measured: model
// tokens settle into usage_ledger, and the MACHINE half had no row anywhere.
//
// A SESSION IS NOT AN OCCUPANCY, which is the whole reason `session_id` had to be added to a table that
// already carried `run_id`. A session is closed for idleness, its machine account is destroyed, and the
// next approval resumes it on whichever machine is free: one session, N holds, and the bill is their SUM.
// Within one hold many runs come and go, so `run_id` cannot key the interval — it stays as it was found.
//
// WHAT THIS FILE DOES NOT DO, stated because a reader will look for it here. It settles nothing into
// usage_ledger (that is occupancy_billing.go's SettleOccupancy, with its own dedupe key) and it runs no
// reaper. What it DID not do until Faz A.4 T5 was refuse anything: AcquireLease now reads
// `runners.capacity` and a machine already holding that many open occupancies is refused
// (ErrMachineAtCapacity). The ceiling is the first and so far the only comparison anything in this tree
// makes against that column.
//
// AND THE REFUSAL IS NOT YET A PLACEMENT OUTCOME. It is reached in production — HoldMachine is called from
// the orchestrator on every attempt that realizes a workspace — but what the orchestrator DOES with a
// refusal is deliberately still "run this attempt unmetered and say so" (see holdMachine in
// apps/control-plane/internal/execution/machine_occupancy.go, which names the two costs). Turning it into a
// park needs a waker that fires when a SLOT FREES, and the only wake in the tree fires when a machine
// CONNECTS.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// ReleaseReasonIdle is the one release reason the billed interval branches on: an occupancy the idle
// reaper ended is billed to `last_activity_at`, and the quiet tail after it is not charged.
//
// IT IS A SECOND PLACE THE WORD LIVES, so something has to hold the two together: the branch itself is the
// `CASE` in storage/queries/leases.sql and migration 000003's CHECK is what keeps the column's vocabulary
// closed, neither of which knows about this line. TestAnIdleClosedOccupancyBillsToLastActivityNotToTheReaper
// closes a hold with THIS constant and then asserts the stored reason reads "idle", so a constant that
// drifted from the database's word would redden rather than silently bill the tail.
//
// The other three reasons ('closed', 'preempted', 'lost') get no constant: nothing branches on them, so a
// constant would be a symbol with no reader.
const ReleaseReasonIdle = "idle"

// ErrOccupancyNotFound reports that no occupancy with this id is visible to this tenant. It is distinct
// from a release that wrote nothing because the occupancy was ALREADY released: that one is not an error
// at all (see ReleaseLease), and collapsing the two would make a repeated release indistinguishable from
// an id that was never valid.
var ErrOccupancyNotFound = errors.New("occupancy not found")

// ErrMachineUnavailable reports that AcquireLease wrote no row: the machine or the session is not one
// this tenant can be served by. It is an ERROR rather than an empty id because a caller can ignore a
// string — the lesson ErrRunPoolNotRecordable in this package was written for, where a write that matched
// nothing reported success and two runs sat unreachable for thirty-one hours.
var ErrMachineUnavailable = errors.New("machine is not one this tenant can occupy")

// ErrMachineAtCapacity reports that AcquireLease wrote no row for the OTHER reason: the machine is real,
// this tenant may occupy it, and its open occupancies already fill `runners.capacity`.
//
// IT IS A SEPARATE SENTINEL FROM ErrMachineUnavailable BECAUSE THE TWO DECIDE OPPOSITE THINGS. An
// unavailable machine is a configuration fault — the id is wrong, or it belongs to somebody else — and it
// will still be wrong on the next attempt. A full machine is a TRANSIENT answer that becomes untrue the
// moment a hold settles. Collapsing them would leave a caller unable to tell "never" from "not yet", which
// is the same distinction errPoolNotServable was split out of the capacity park for.
var ErrMachineAtCapacity = errors.New("machine already holds as many occupancies as its capacity allows")

// Occupancy is one row of runner_leases: one session's hold on one machine.
type Occupancy struct {
	ID        string
	RunnerID  string
	SessionID string
	// StartedAt is when the hold began. LastActivityAt is the last moment this session did anything on the
	// machine — the idle reaper reads it to know when to fire, and the bill reads it to know where to stop.
	StartedAt      time.Time
	LastActivityAt time.Time
	// ReleasedAt is the zero time for as long as the hold is open.
	ReleasedAt    time.Time
	ReleaseReason string
	// Billed is the interval this occupancy costs, derived in SQL from the three timestamps above rather
	// than stored. A stored duration would be a second truth that can disagree with the one it came from.
	// For an occupancy that is still open it is the elapsed time so far, which is a statement about a hold
	// in progress and not a settled amount.
	Billed time.Duration
}

func newLeaseID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate lease id: %w", err)
	}
	return "lse_" + hex.EncodeToString(raw[:]), nil
}

// AcquireLease opens an occupancy: this session now holds this machine, IF the machine has a slot free.
// The id is minted HERE and never taken from a caller, which is the same rule the runner registry applies
// to a machine's own id.
//
// A machine or a session this tenant cannot see writes NO row and returns ErrMachineUnavailable — the
// statement selects both out of their own tables, so row-level security decides, rather than a predicate
// this package would have to remember to write.
//
// A MACHINE ALREADY HOLDING `runners.capacity` OPEN OCCUPANCIES RETURNS ErrMachineAtCapacity (Faz A.4 T5),
// AND THAT REFUSAL IS THE STATEMENT'S, NOT THIS FUNCTION'S. The ceiling is a subquery inside AcquireLease's
// own INSERT and the answer is read from whether a row came back, because a ceiling checked here — count,
// decide, then write — is not a ceiling at all: under READ COMMITTED two callers taking the last slot at
// once both read the machine one short of full and both write, the machine goes over what it declared, and
// neither caller is ever told, because both were told yes.
//
// THE TRANSACTION EXISTS FOR THE LOCK AND THE LOCK EXISTS FOR THE TIE. LockMachineForPlacement takes the
// machine's row `FOR UPDATE`, so contenders for one machine are ordered; the loser blocks there and the
// INSERT it then runs takes a FRESH snapshot in which the winner's row is committed. Both statements in
// one transaction is what makes that ordering mean anything.
//
// AND THE ZERO-ROW INSERT IS AMBIGUOUS, so it is ASKED rather than guessed — the pattern RecordRunPool and
// SettleOccupancy in this package already use. Full and unavailable are opposite answers: one becomes
// untrue the moment a hold settles, the other will be just as true on the next attempt. The re-read happens
// while the machine's row is still locked, so it reports the number the INSERT judged against.
func (s *Store) AcquireLease(ctx context.Context, tenant Tenant, sessionID, runnerID string) (string, error) {
	if sessionID == "" || runnerID == "" {
		return "", errors.New("a session and a machine are required to open an occupancy")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	leaseID, err := newLeaseID()
	if err != nil {
		return "", err
	}
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", fmt.Errorf("begin acquire lease on %s: %w", runnerID, err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	// The machine, locked. No row here is already an answer — this tenant cannot see the machine at all —
	// and answering it now costs the INSERT below nothing and spares the ambiguity a step.
	var capacity int
	switch err := tx.QueryRow(ctx, storage.Query("LockMachineForPlacement"), runnerID, tenant.Project).Scan(&capacity); {
	case errors.Is(err, pgx.ErrNoRows):
		return "", fmt.Errorf("%w: machine %s", ErrMachineUnavailable, runnerID)
	case err != nil:
		return "", fmt.Errorf("lock machine %s for placement: %w", runnerID, err)
	}

	var stored string
	switch err := tx.QueryRow(ctx, storage.Query("AcquireLease"),
		leaseID, tenant.Project, runnerID, sessionID).Scan(&stored); {
	case errors.Is(err, pgx.ErrNoRows):
		// The machine is visible, so the two remaining reasons are its ceiling and the session. Ask.
		var open int
		// ONE argument: the count is a property of the MACHINE, not of this tenant. It used to carry the
		// project and that made it wrong on a shared machine — see the statement's own paragraph.
		if rerr := tx.QueryRow(ctx, storage.Query("MachineOpenOccupancies"), runnerID).Scan(&open); rerr != nil {
			return "", fmt.Errorf("read the open occupancies of machine %s: %w", runnerID, rerr)
		}
		// The statement's own guard, ASKED rather than restated: a machine that declared nothing has no
		// ceiling, so it can never be the reason a row was refused, and a bare `open >= capacity` would name
		// it as one for every refusal. It goes through HasRoom because that predicate now has a second
		// reader (the placement preference), and two hand-written copies of a rule this narrow is how the
		// two come to disagree about what "full" means.
		if !(PoolMachineLoad{Open: open, Capacity: capacity}).HasRoom() {
			return "", fmt.Errorf("%w: machine %s holds %d of %d", ErrMachineAtCapacity, runnerID, open, capacity)
		}
		return "", fmt.Errorf("%w: session %s", ErrMachineUnavailable, sessionID)
	case err != nil:
		return "", fmt.Errorf("acquire lease on %s: %w", runnerID, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit acquire lease on %s: %w", runnerID, err)
	}
	return stored, nil
}

// PoolMachineLoad is one machine's placement weight: the holds open on it right now, and the ceiling it
// declared. `Capacity` is the column's raw value, so ZERO means the machine declared nothing and has no
// ceiling at all — never "full". Reading it the other way round is the one real trap in this column and is
// the reason it is carried here rather than folded into a single "has room" bool by the query.
type PoolMachineLoad struct {
	RunnerID string
	Open     int
	Capacity int
}

// HasRoom reports whether this machine can still take a session it is not already carrying.
//
// IT IS THE ONE PLACE THE ZERO RULE IS WRITTEN IN GO, and it exists because the rule had started to be
// copied. `r.capacity = 0 OR count < r.capacity` is AcquireLease's own guard, this method is what the
// refusal arm below and the placement preference in
// apps/control-plane/internal/execution/runner_least_loaded.go both ask instead of restating it, and a
// fourth copy is what would eventually rank an undeclared machine as permanently full — which, since no
// machine in any shipped deployment declares a capacity, would read the ENTIRE fleet as full.
//
// `Capacity <= 0` rather than `== 0` so the negation is exactly the `capacity > 0` the acquire's arm has
// always used. Nothing can store a negative anyway (migration 000005 left `CHECK (capacity >= 0)`), so the
// two agree on every value that exists; the form is chosen so a reader diffing them finds no difference.
func (l PoolMachineLoad) HasRoom() bool {
	return l.Capacity <= 0 || l.Open < l.Capacity
}

// PoolMachineLoads reports how loaded each machine in one pool is, so a placement can PREFER the emptiest
// one. It is the read behind "open the session on the emptiest Mac".
//
// IT DECIDES NOTHING AND MUST NOT, which is why it is a plain read outside any transaction while
// AcquireLease's count of the very same rows is a subquery inside the write. Two callers ranking a fleet
// from this answer at the same moment both see the same emptiest machine and both go for it; the one that
// loses is refused by the ceiling, not by anything here. A version of this that took a lock and reserved a
// slot would be a second ceiling — weaker than the real one and disagreeing with it under contention, which
// is the shape apps/control-plane/internal/execution/orchestrator.go names and refuses.
//
// AN UNKNOWN MACHINE IS ABSENT FROM THE ANSWER RATHER THAN ZERO, and the caller has to decide what absence
// means. Nothing is invented here for a machine the tenant cannot see: a pool with no rows answers empty,
// which is the honest thing for a stack whose gateway has no registry behind it.
func (s *Store) PoolMachineLoads(ctx context.Context, tenant Tenant, poolID string) ([]PoolMachineLoad, error) {
	if tenant.Project == "" || poolID == "" {
		return nil, errors.New("a tenant and a pool are required to weigh a pool's machines")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("PoolMachineOccupancies"), tenant.Project, poolID)
	if err != nil {
		return nil, fmt.Errorf("read the machine loads of pool %s: %w", poolID, err)
	}
	defer rows.Close()
	var out []PoolMachineLoad
	for rows.Next() {
		var load PoolMachineLoad
		if err := rows.Scan(&load.RunnerID, &load.Capacity, &load.Open); err != nil {
			return nil, fmt.Errorf("scan a machine load of pool %s: %w", poolID, err)
		}
		out = append(out, load)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the machine loads of pool %s: %w", poolID, err)
	}
	return out, nil
}

// TouchLease records that this session is still using the machine. It is a no-op on an occupancy that has
// already been released, and deliberately not an error: the touch races the reaper by construction, so a
// call that lands just after the release is ordinary rather than exceptional. What it must never do is
// move `last_activity_at` on a closed hold, because that changes a bill that was already settled.
//
// A TOUCH THAT MATCHED NO ROW AT ALL IS ErrOccupancyNotFound, and the distinction is the reason this reads
// the row instead of returning nil on any zero-row update. The two no-matches mean opposite things: an
// already-released hold is the ordinary race above, while an id nothing answers to means the caller is
// keeping alive an occupancy that does not exist — and reporting success for that is how a live session
// gets reaped as idle while every touch it sent returned nil.
func (s *Store) TouchLease(ctx context.Context, tenant Tenant, leaseID string) error {
	if leaseID == "" {
		return errors.New("an occupancy id is required")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tag, err := s.pool.Exec(ctx, storage.Query("TouchLease"), leaseID, tenant.Project)
	if err != nil {
		return fmt.Errorf("touch lease %s: %w", leaseID, err)
	}
	if tag.RowsAffected() == 0 {
		if _, rerr := s.Occupancy(ctx, tenant, leaseID); rerr != nil {
			return rerr
		}
	}
	return nil
}

// ReleaseLease closes an occupancy with the reason it closed. The reason decides the bill:
// ReleaseReasonIdle bills to `last_activity_at`, everything else to the release itself.
//
// DO NOT CALL THIS TO END A HOLD. SettleOccupancy IS THE ONLY WAY TO CLOSE ONE, and this function's
// having no production caller is a decision, not an oversight waiting to be corrected.
//
// It closes and does NOTHING ELSE, and the two things it omits are both irreversible:
//
//   - IT BILLS NOTHING. SettleOccupancy closes and settles the machine time IN ONE TRANSACTION; this
//     writes `released_at` alone. Afterwards nothing can tell the row apart from one that settled
//     correctly — `released_at` is set either way — so the lost revenue is not merely lost, it is
//     undetectable. That is the whole reason the settle is one transaction rather than two calls.
//   - IT ANNOUNCES NOTHING. SettleOccupancy calls announceFreedSlot (Faz A.4 T6), which is what wakes a
//     run parked waiting for capacity. A hold closed here frees a slot that nothing is told about, and
//     the parked run keeps waiting for a machine that is already free.
//
// COUNTED 2026-08-05, because "no caller" is a claim about the whole tree and decays silently:
//
//	grep -rn '\.ReleaseLease(' --include='*.go' . | grep -v node_modules   -> 1 hit, and it is a TEST
//
// That one is fleetEnv.mustRelease in tests/component/fleet/lease_lifecycle_test.go, used at three call
// sites in that one file to drive the close path directly. Production reaches the close only through
// SettleOccupancy, whose own callers are three: HoldMachine's "lost" arm, the idle release's timely
// settle, and the recovery sweep for the settle that failed.
//
// What is NOT dead is the SQL. storage/queries/leases.sql's ReleaseLease statement has two Go callers —
// this function and SettleOccupancy's transaction — so the statement is live and load-bearing, and a
// reader who greps the name finds production use. The unused thing is this METHOD.
//
// IT IS WRITE-ONCE AND A REPEAT IS NOT AN ERROR, and the pair is the point. Write-once because moving
// `released_at` forward extends the bill, so a redelivered release must change nothing. Not-an-error
// because a release is repeated by ordinary means — a retried command, a reaper meeting a session that
// closed itself — and turning that into a failure would make callers invent their own suppression.
//
// A no-row update is ambiguous between those two, so the ambiguous case READS the row instead of guessing:
// already released is nil, and no row at all is ErrOccupancyNotFound. It costs one extra round trip on a
// path that runs once per hold.
func (s *Store) ReleaseLease(ctx context.Context, tenant Tenant, leaseID, reason string) error {
	if leaseID == "" || reason == "" {
		return errors.New("an occupancy id and a release reason are required")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var releasedAt time.Time
	switch err := s.pool.QueryRow(ctx, storage.Query("ReleaseLease"),
		leaseID, tenant.Project, reason).Scan(&releasedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		if _, rerr := s.Occupancy(ctx, tenant, leaseID); rerr != nil {
			return rerr
		}
		return nil // already released: the first close is the one that counts
	case err != nil:
		return fmt.Errorf("release lease %s: %w", leaseID, err)
	}
	return nil
}

// Occupancy reads one hold, with its billed interval derived by the statement rather than recomputed here.
// The rule lives in SQL so that the list read below and this one cannot drift into two answers.
func (s *Store) Occupancy(ctx context.Context, tenant Tenant, leaseID string) (Occupancy, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	row := s.pool.QueryRow(ctx, storage.Query("GetOccupancy"), leaseID, tenant.Project)
	out, err := scanOccupancy(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Occupancy{}, fmt.Errorf("%w: %s", ErrOccupancyNotFound, leaseID)
	}
	if err != nil {
		return Occupancy{}, fmt.Errorf("read occupancy %s: %w", leaseID, err)
	}
	return out, nil
}

// SessionOccupancies returns every machine this session has held, oldest first. This IS the session's
// machine bill: the sum of these intervals, and never the span from the first hold to the last, which
// would charge the customer for the gaps in which they held nothing.
func (s *Store) SessionOccupancies(ctx context.Context, tenant Tenant, sessionID string) ([]Occupancy, error) {
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	rows, err := s.pool.Query(ctx, storage.Query("SessionOccupancies"), tenant.Project, sessionID)
	if err != nil {
		return nil, fmt.Errorf("read occupancies of session %s: %w", sessionID, err)
	}
	defer rows.Close()
	var out []Occupancy
	for rows.Next() {
		occupancy, err := scanOccupancy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan occupancy of session %s: %w", sessionID, err)
		}
		out = append(out, occupancy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read occupancies of session %s: %w", sessionID, err)
	}
	return out, nil
}

// MachineOccupancies is ONE MACHINE's session history, newest first — what it is holding now and what it
// held before. It is the read behind the panel's machine-detail view (device plan T6, DoD 10), which is
// the question an operator asks before unplugging a Mac.
//
// ‼️ IT IS KEYSET-PAGINATED ON (started_at, id) AND THE PAIR IS THE POINT. `started_at` alone ties
// whenever two holds begin in the same clock tick, and a LIMIT over a tie is a page that can drop or
// repeat a row between requests. This tree has had an unordered LIMIT decide a security outcome twice;
// a paginated history is the same hazard wearing a UI.
//
// A zero `before` starts at the newest row. The caller passes the last row of the previous page to
// continue, which is why both halves of the cursor are parameters rather than one.
func (s *Store) MachineOccupancies(ctx context.Context, tenant Tenant, runnerID string, before time.Time, beforeID string, limit int) ([]Occupancy, error) {
	if limit <= 0 {
		limit = 50
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	var cursor any
	if !before.IsZero() {
		cursor = before
	}
	rows, err := s.pool.Query(ctx, storage.Query("MachineOccupancies"), runnerID, tenant.Project, cursor, beforeID, limit)
	if err != nil {
		return nil, fmt.Errorf("read occupancies of machine %s: %w", runnerID, err)
	}
	defer rows.Close()
	var out []Occupancy
	for rows.Next() {
		occupancy, err := scanOccupancy(rows)
		if err != nil {
			return nil, fmt.Errorf("scan occupancy of machine %s: %w", runnerID, err)
		}
		out = append(out, occupancy)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read occupancies of machine %s: %w", runnerID, err)
	}
	return out, nil
}

// scanOccupancy is the ONE place the occupancy column list is read, shared by the single read and the list
// read above — two hand-written copies is how they would come to return different shapes of the same row.
// It takes this package's existing scanRow (publication.go), which is what a pgx.Row and a pgx.Rows have in
// common.
func scanOccupancy(row scanRow) (Occupancy, error) {
	var (
		out        Occupancy
		releasedAt *time.Time
		seconds    float64
	)
	if err := row.Scan(&out.ID, &out.RunnerID, &out.SessionID, &out.StartedAt, &out.LastActivityAt,
		&releasedAt, &out.ReleaseReason, &seconds); err != nil {
		return Occupancy{}, err
	}
	if releasedAt != nil {
		out.ReleasedAt = *releasedAt
	}
	out.Billed = time.Duration(seconds * float64(time.Second))
	return out, nil
}
