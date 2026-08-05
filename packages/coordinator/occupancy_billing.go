package coordinator

// THE MONEY HALF OF A MACHINE OCCUPANCY (Faz A.4 T4).
//
// occupancy.go is the durable record — take a machine, keep it alive, give it back. This file is the two
// questions that record exists to answer once money depends on it: WHO IS HOLDING A MACHINE RIGHT NOW,
// and WHAT DID A HOLD THAT ENDED COST. It is a separate file rather than more of occupancy.go because the
// two have different readers: the record is read by placement and by the reaper, and this is read by the
// bill.
//
// UNTIL THIS FILE, EVERY WRITER T1 SHIPPED HAD ZERO PRODUCTION CALLERS:
//
//	for f in AcquireLease TouchLease ReleaseLease SessionOccupancies; do
//	  grep -rn "\.$f(" --include='*.go' apps cmd packages adapters | grep -v _test | grep -v 'func ' | wc -l
//	done
//	→ 0 0 0 0        (2026-08-05, before this file)
//	grep -rn 'machine\.minutes' --include='*.go' . | grep -v node_modules | wc -l
//	→ 0              (same measurement)
//
// So a Mac was held, handed back, and held again, and not one row said so. Token spend settled into
// usage_ledger on every model step while the machine half — the half a Mac costs real money for — was
// measured nowhere.
//
// WHAT IS BILLED, AND THE ONE PLACE THE RULE LIVES. A session occupies a machine N times and the bill is
// the SUM of those occupancies, never the session's wall clock: the gaps in which it held nothing are not
// the customer's. Within one occupancy the interval is
//
//	closed by idle : last_activity_at - started_at    (the quiet tail is the platform's cost)
//	any other way  : released_at      - started_at
//
// and that `CASE` is in storage/queries/leases.sql, evaluated by the database. Nothing here recomputes it.
// In particular NOTHING HERE SUBTRACTS A TTL: a TTL is a setting so it changes, and a reaper is a sweep so
// it fires late, and either error is an error about money.

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/palgroup/palai/storage"
)

// ReleaseReasonLost is the reason a hold nobody witnessed the end of is closed with. It is a constant for
// the same reason ReleaseReasonIdle is one — HoldMachine below branches to it, so the word has a writer in
// Go as well as a CHECK in migration 000003_lease_occupancy — and unlike ReleaseReasonIdle it changes no
// arithmetic: the billing `CASE` in storage/queries/leases.sql names 'idle' and treats every other reason
// alike.
const ReleaseReasonLost = "lost"

// OpenOccupancy returns the session's currently-open hold on a machine, if it has one.
//
// IT IS DERIVED FROM SessionOccupancies RATHER THAN FROM A STATEMENT OF ITS OWN, which costs a session's
// whole occupancy history to answer a question about its last row. That is deliberate at this size: a
// session's holds are counted in ones, the read runs once per attempt and once per idle release, and the
// alternative is a second statement whose predicate could drift from the one the bill is read through.
// The day a session accumulates enough holds for this to matter, the fix is a `released_at IS NULL`
// statement beside the others in storage/queries/leases.sql, not a cache here.
//
// The LAST open row wins if somehow there are several. There should never be: acquisition happens once per
// realized allocation, under the workspace writer lease, and a release closes the row before the next
// acquire opens one. It is a total rule rather than a panic because an extra open row is a placement
// problem for whoever finds it, and answering "the newest" is what a caller keeping a hold alive means.
func (s *Store) OpenOccupancy(ctx context.Context, tenant Tenant, sessionID string) (Occupancy, bool, error) {
	held, err := s.SessionOccupancies(ctx, tenant, sessionID)
	if err != nil {
		return Occupancy{}, false, err
	}
	var out Occupancy
	found := false
	for _, occupancy := range held {
		if occupancy.ReleasedAt.IsZero() {
			out, found = occupancy, true
		}
	}
	return out, found, nil
}

// StrandedOccupancy names a hold whose closer never ran: open, on a machine its session has demonstrably
// handed back. It carries its own project because the read that produces it spans every tenant.
type StrandedOccupancy struct {
	ID        string
	Project   string
	SessionID string
}

// StrandedOccupancies returns up to limit holds that are still open on a machine that was handed back.
//
// IT IS THE SECOND HALF OF A ONE-WAY DOOR, and without it the first half is a permanent leak. The idle
// release moves the workspace to `paused` BEFORE it settles, so a settle that fails leaves a hold that
// nothing can reach: the idle candidate query wants `ready`, the writer-lease reclaim answers a different
// question about a different table, and ReleaseLease has no production caller on purpose. This read is what
// makes that failure cost a tick instead of a machine slot. The predicate, and why each half of it is
// there, is in storage/queries/leases.sql.
//
// A MAINTENANCE READ SPANNING EVERY TENANT, like IdleWorkspacesForRelease and AbandonedWriterLeases — so it
// runs under the system scope rather than a tenant one, and each row carries its own project for the
// tenant-scoped settle that follows.
//
// A NON-POSITIVE LIMIT IS REFUSED rather than clamped, for the reason the sibling maintenance reads refuse
// a negative interval: a caller that passed one meant something, and silently returning nothing would read
// as "no holds are stranded" — the one answer this function must never give when it did not look.
func (s *Store) StrandedOccupancies(ctx context.Context, limit int) ([]StrandedOccupancy, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("stranded occupancy batch must be positive, got %d", limit)
	}
	ctx = storage.WithSystemScope(ctx)
	rows, err := s.pool.Query(ctx, storage.Query("StrandedOccupancies"), limit)
	if err != nil {
		return nil, fmt.Errorf("read stranded occupancies: %w", err)
	}
	defer rows.Close()
	var out []StrandedOccupancy
	for rows.Next() {
		var c StrandedOccupancy
		if err := rows.Scan(&c.ID, &c.Project, &c.SessionID); err != nil {
			return nil, fmt.Errorf("scan stranded occupancy: %w", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read stranded occupancies: %w", err)
	}
	return out, nil
}

// HoldMachine records that this session is on this machine right now, and returns the occupancy id.
//
// IT IS OPEN-OR-KEEP-ALIVE AND NOT PLAIN ACQUIRE, because an occupancy is not an attempt. A session's hold
// spans every run that lands on the same allocation, so calling this once per attempt must open ONE row
// and then touch it — a row per attempt would bill a busy session once per message and would make
// `SessionOccupancies` a run log instead of a machine bill.
//
// A HOLD THAT NAMES A DIFFERENT MACHINE IS CLOSED, and this is the branch worth reading twice. Reaching
// here means the session is demonstrably on THIS machine now, so the older hold has ended — the release
// that should have closed it did not, or the allocation was rebuilt somewhere else. Leaving it open would
// leave a row that never settles: unbilled machine time, and (from T5 on) a machine placement believes is
// occupied forever. It closes as 'lost', which is the vocabulary's word for a hold whose end nobody
// witnessed, and it SETTLES on the way out so the interval that did happen is still billed.
func (s *Store) HoldMachine(ctx context.Context, tenant Tenant, sessionID, runnerID string) (string, error) {
	if sessionID == "" || runnerID == "" {
		return "", errors.New("a session and a machine are required to hold a machine")
	}
	open, found, err := s.OpenOccupancy(ctx, tenant, sessionID)
	switch {
	case err != nil:
		return "", err
	case found && open.RunnerID == runnerID:
		if err := s.TouchLease(ctx, tenant, open.ID); err != nil {
			return "", err
		}
		return open.ID, nil
	case found:
		if _, err := s.SettleOccupancy(ctx, tenant, open.ID, ReleaseReasonLost); err != nil {
			return "", fmt.Errorf("close the hold session %s left on machine %s: %w", sessionID, open.RunnerID, err)
		}
	}
	return s.AcquireLease(ctx, tenant, sessionID, runnerID)
}

// SettleOccupancy closes an occupancy and settles its billed machine time into the ledger IN ONE
// TRANSACTION. It reports whether THIS call is the one that closed it.
//
// ONE TRANSACTION IS THE WHOLE POINT and is why this exists rather than a ReleaseLease followed by a
// settle. Two transactions have a window: a crash between them leaves a hold that is closed and a machine
// that was never billed, and nothing afterwards can tell that row apart from one that settled correctly —
// `released_at` is set either way. Closed and billed land together or neither does.
//
// A REPEATED RELEASE SETTLES NOTHING NEW, twice over. The UPDATE is write-once (`released_at IS NULL`), so
// a second call matches no row and returns false before reaching the ledger at all; and the ledger row's
// dedupe key is the OCCUPANCY's own id, so even a settle that did run twice collides on
// `(project_id, dedupe_key)` and inserts once (BIL-001). The second guard is not redundant with the first:
// they cover different callers — one repeated release, and two paths racing to close the same hold.
//
// THE QUANTITY IS READ BACK FROM THE DATABASE, not computed here from the timestamps this call just wrote.
// The billing rule is the `CASE` in GetOccupancy, and a copy of it in Go would be a second answer to the
// only question this file exists to answer.
//
// A ZERO-LENGTH HOLD SETTLES NO ROW, because settleUsage skips a zero quantity — an entry that prices to
// nothing only dilutes the ledger. Say it out loud because it makes an absence ambiguous: "no ledger row"
// means EITHER not settled OR settled zero, and a test that asserts an absence has to make the hold long
// enough that the two are different.
//
// AND A CLOSED HOLD ANNOUNCES THE SLOT IT FREED (Faz A.4 T6). This is the one function in the tree that
// ends an occupancy — measured, not assumed: `released_at` is moved by exactly one statement
// (storage/queries/leases.sql ReleaseLease), and its only two Go callers are this and the bare
// `ReleaseLease`, which has no production caller on purpose. Its own three callers are the idle release's
// timely settle, the recovery sweep for the settle that failed, and HoldMachine's "lost" arm. Waking here
// therefore covers all three STRUCTURALLY, where three hand-wired call sites would have covered whichever
// two somebody remembered — and the one most easily forgotten, the lost arm, is the one no sweep watches.
func (s *Store) SettleOccupancy(ctx context.Context, tenant Tenant, leaseID, reason string) (bool, error) {
	if leaseID == "" || reason == "" {
		return false, errors.New("an occupancy id and a release reason are required")
	}
	ctx = storage.ScopeToTenant(ctx, tenant.Project)
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("begin settle occupancy: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	var releasedAt time.Time
	switch err := tx.QueryRow(ctx, storage.Query("ReleaseLease"), leaseID, tenant.Project, reason).Scan(&releasedAt); {
	case errors.Is(err, pgx.ErrNoRows):
		// Nothing was closed here. Which of the two that is decides what the caller is told, exactly as
		// ReleaseLease decides it: an id nothing answers to is an error, and a hold somebody else already
		// closed is the ordinary case — its bill is settled and belongs to the call that closed it.
		if _, rerr := s.Occupancy(ctx, tenant, leaseID); rerr != nil {
			return false, rerr
		}
		return false, nil
	case err != nil:
		return false, fmt.Errorf("close occupancy %s: %w", leaseID, err)
	}

	closed, err := scanOccupancy(tx.QueryRow(ctx, storage.Query("GetOccupancy"), leaseID, tenant.Project))
	if err != nil {
		return false, fmt.Errorf("read the closed occupancy %s: %w", leaseID, err)
	}
	// runID is empty and that is the honest field, not a missing one: one occupancy hosts many runs, so
	// naming any of them would attribute a machine's whole hold to whichever run happened to be last.
	if err := settleUsage(ctx, tx, tenant, usageEntry{
		sessionID: closed.SessionID,
		meter:     meterMachineMinutes, unit: unitMinute,
		dedupeKey: "lease:" + leaseID + ":" + meterMachineMinutes,
		quantity:  closed.Billed.Minutes(),
	}); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit settled occupancy %s: %w", leaseID, err)
	}
	s.announceFreedSlot(ctx, tenant, leaseID, closed.RunnerID)
	return true, nil
}

// announceFreedSlot re-enters the oldest run parked on this machine's pool, because a hold just ended and
// that is the only event a capacity ceiling makes room by.
//
// IT RUNS AFTER THE COMMIT AND IT SWALLOWS ITS ERRORS, which is the same stance the gateway's own
// wakeParkedRun takes and it is a priority statement rather than laziness. Closing and BILLING a hold is
// the settle's job; announcing it is work done on the side. Inside the transaction, a wake that could not
// answer would roll back a settlement that had already succeeded — a machine left occupied and a customer
// left unbilled because some other tenant's parked run could not be read. What a post-commit wake costs
// instead is a crash in the window: the slot is free and nothing announced it. That is recoverable by
// construction and by three different events — the next hold to settle on the pool, the machine's next
// connect (a runner re-dials after every lease), and PALAI_FLEET_PARK_TTL where an operator set one.
//
// THE POOL IS READ RATHER THAN CARRIED because an occupancy names a MACHINE and a park names a POOL, and
// nothing in between stores both. A machine deleted since the hold opened answers no row, and then this
// announces nothing: guessing a pool would re-enter a run somewhere no machine of its tenant will come.
//
// IT IS POOL-KEYED AND NOT MACHINE-KEYED, and that is a decision with a cost that was measured rather
// than waved through. The freed slot is on ONE machine; the wake re-enters the pool's oldest parked run,
// which may dial a different machine of that pool and find it full. The alternative — a wake that
// reserved this machine for that run — would be a second notion of "assigned to" beside the durable one
// the job queue already is. What makes the cheap version correct is that a park REFUNDS the attempt it
// consumed, so a wake that ends in another park costs the run nothing at all; without that refund this
// would have to be machine-keyed, because every spurious wake would be a step toward a dead letter.
func (s *Store) announceFreedSlot(ctx context.Context, tenant Tenant, leaseID, runnerID string) {
	if runnerID == "" {
		return
	}
	var poolID string
	switch err := s.pool.QueryRow(ctx, storage.Query("MachinePool"), runnerID, tenant.Project).Scan(&poolID); {
	case errors.Is(err, pgx.ErrNoRows):
		return // the machine is gone; a slot on a pool nobody can name announces nothing
	case err != nil:
		log.Printf("occupancy %s settled but the pool of machine %s could not be read, so no parked run was woken: %v",
			leaseID, runnerID, err)
		return
	}
	if _, err := s.WakeRunAwaitingCapacity(ctx, tenant, poolID); err != nil {
		log.Printf("occupancy %s freed a slot on pool %s and no parked run was woken: %v", leaseID, poolID, err)
	}
}
