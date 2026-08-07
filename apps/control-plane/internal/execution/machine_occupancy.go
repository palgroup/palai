package execution

// WHO IS HOLDING A MACHINE, from the orchestrator's side (A.4 T4).
//
// packages/coordinator/occupancy.go has recorded an occupancy — one session's hold on one machine — since
// T1, and packages/coordinator/occupancy_billing.go settles what a closed hold cost. Neither had a
// production caller: measured 2026-08-05, `AcquireLease`, `TouchLease`, `ReleaseLease` and
// `SessionOccupancies` were each called from exactly zero non-test files. This file and idle_release.go
// are the two ends that make them happen — the machine half of the bill was built and not wired, which in
// this tree is a fully-tested surface that never runs.
//
// THE OCCUPANCY IS THE ALLOCATION'S HOLD, NOT THE ATTEMPT'S, and that choice decides everything else here.
// What a session actually keeps on a Mac between messages is its allocation directory and its uid slot;
// they survive every attempt boundary and are reclaimed by exactly one thing, the idle releaser. So the
// interval with a beginning and an end is the ALLOCATION's, and it opens where the allocation is realized
// on a machine and closes where the releaser hands it back. An attempt-scoped occupancy would have to be
// opened and closed per message and would bill a conversation once per turn.
//
// HONEST CEILING — A RUN WITH NO WORKSPACE OCCUPIES A MACHINE AND IS NOT METERED. A run whose session has
// no repository binding never plans a workspace, so nothing below is reached, and yet its attempt really
// does hold a machine for as long as it runs. That time is unbilled today. It is named rather than papered
// over because the alternative — opening an occupancy at the dial — would open holds that nothing ever
// closes: the idle releaser is driven by workspaces, so a workspace-less session has no reclaim path and
// its row would stay open forever, which is worse than being absent and much harder to notice.
//
// WHICH machine is not a new seam: `machineOf` (machine_call.go) already derives it from the connection,
// the same way a background task records the machine its reconciler must address later. A channel that
// reaches no machine answers "", which is the honest answer for the deterministic e2e tier and every
// hand-built component orchestrator — there is no machine there to occupy.

import (
	"context"
	"errors"
	"log"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// holdMachine records that this session is occupying this attempt's machine, and returns the occupancy id
// to keep alive at attempt end. It returns ("", nil) when there is nothing to record — no machine on this
// connection, or a store that refused — and every caller treats that as "no occupancy", never as an error.
//
// A FAILURE HERE MUST NOT FAIL THE ATTEMPT, and that is a billing decision made deliberately in the
// customer's favour. The work is provisioned, the machine is dialled, and the human is waiting; refusing to
// run it because a metering row would not write trades a working product for an accounting entry. What it
// costs is real and is stated: an occupancy that did not open is machine time nobody is charged for. The
// log line is the record, and the NEXT attempt on the same allocation opens the hold that this one missed
// — HoldMachine opens a row whenever the session has none, so the loss is bounded by one attempt rather
// than by the life of the session.
//
// A CAPACITY REFUSAL IS NOT ONE OF THOSE FAILURES AND IS RETURNED RATHER THAN LOGGED AWAY (Faz A.4 T5/T6).
// Every other error here means the metering could not be WRITTEN; ErrMachineAtCapacity means it was
// REFUSED — the machine is already holding as many sessions as `runners.capacity` allows, and this attempt
// is about to be the one over. Collapsing the two would send the next reader to look for a database blip,
// which is the failure this tree keeps recording as an assertion pointing at the wrong file; and it would
// also decide the attempt's fate wrongly, because these two have OPPOSITE right answers. A metering row
// that will not write is the customer's gain and the attempt proceeds. A machine that says it is full is a
// placement fact, and running anyway puts a session on a Mac that had declared it would not take it — over
// the ceiling, and unbilled, so the overrun appears in no row at all.
//
// WHAT THE CALLER DOES WITH THE REFUSAL: it parks the run (ExecuteAttempt). What is parked is precisely
// what has a wake — see the record below.
//
// AND A SESSION ALREADY ON THIS MACHINE IS NEVER PARKED, WHATEVER THE MACHINE'S COUNT SAYS. HoldMachine is
// open-or-keep-alive: a session whose open hold names this machine is TOUCHED and never reaches
// AcquireLease, so the ceiling is not consulted for it at all. That is the right answer and not a gap —
// the ceiling counts how many DIFFERENT sessions a machine carries at once, and a conversation already on
// a Mac is not a new one arriving. Parking it would evict a thread from the machine holding its
// allocation, which is the opposite of what a ceiling is for.
//
// ---
//
// THE RECORD, 2026-08-05: BETWEEN A.4 T5 AND T6 THIS FUNCTION LOGGED THE REFUSAL AND RAN THE ATTEMPT
// ANYWAY, and that was a ceiling held deliberately rather than an unfinished branch. It is written down as
// a DATE rather than as a ceiling because all three of its reasons have since closed, and a ceiling
// sentence that outlives its reasons is read as live guidance by the next person:
//
//   - NOTHING WOKE A RUN WHEN A SLOT FREED. The only capacity wake fired from the gateway's handleConnect,
//     when a machine CONNECTS — an event that says nothing about whether a slot exists, and one a fleet
//     whose machines stay connected may not produce for hours. CLOSED by T6: SettleOccupancy announces the
//     slot it freed, and every closer in the tree goes through it (its own comment carries the grep).
//   - A PARK COST THE RUN AN ATTEMPT, so a run woken onto a still-full pool walked toward a dead letter
//     five waits at a time. CLOSED by T6: ParkRunForCapacity refunds the attempt its claim consumed.
//   - A HOLD COULD BE SLOW TO CLOSE, and a settle that FAILED left one open with no route back at all —
//     so a declared fleet lost a slot per failed settle, permanently. CLOSED before T6 by
//     IdleReleaser.settleStranded (the recovery sweep) and by lease_reclaim.go's dangling-lease sweep;
//     what remains is a delay bounded by the sweep interval.
//
// THERE ARE TWO REFUSALS HERE AND THEY HAVE DIFFERENT REACH — count them rather than reading one sentence
// about "the refusal", because until 2026-08-07 there WAS one sentence and it was true of only one of them:
//
//   - THE CAPACITY REFUSAL STILL CANNOT FIRE ON ANY SHIPPED DEPLOYMENT, and that is a live ceiling rather
//     than a date. No machine declares a capacity — the runner sends the field only when an operator
//     configures one — so `runners.capacity` is 0 everywhere and the ceiling in leases.sql never binds.
//     This branch exists for the first deployment that opts in.
//   - THE TENANCY REFUSAL NEEDS NO CONFIGURATION AT ALL AND FIRES TODAY (000008). `grep -c unmetered` over
//     this stack's control-plane logs → 5, every one of them this refusal, on 2026-08-06 and 2026-08-07.
//
// AND THAT SECOND ONE REACHED A `return "", nil` AND RAN THE ATTEMPT ANYWAY. What it cost is the thing
// 000008 exists to prevent, so it is written down rather than quietly repaired: a session whose Mac was
// held by ANOTHER customer executed on that Mac — measured on ses_a6dc5d6e at 04:19:26, which went on to
// clone a repository and run `palai.workspace.shell` there — and it opened no row, so the crossing was
// invisible on the bill as well as to the guard. ExecuteAttempt's park arm named this error the whole
// time; it could not fire because this function answered `nil` before the error could reach it. A guard
// declared, routed, commented and UNREACHABLE — the same shape this tree keeps finding in its own code.
//
// THE GENERIC ARM BELOW STAYS FAIL-OPEN, and the distinction is what makes both defensible: a hold that
// could not be WRITTEN is the meter's failure and must not take the customer's run down with it, while a
// hold that was REFUSED is a placement answer — the same kind of answer as the ceiling above it, and the
// caller already knows what to do with one.
//
// AND IT IS REACHED ONLY FOR A RUN THAT HAS A WORKSPACE, which is an honest limit on the ceiling itself
// rather than on this branch. The occupancy is the ALLOCATION's hold, so ExecuteAttempt calls this inside
// its `wsPlanned` arm; a run whose session has no repository binding occupies a machine for the length of
// its attempt, opens no hold, and therefore cannot be parked by a full machine either. That is the same
// unmetered-attempt ceiling this file has named since A.4 T4, and the alternative — opening a hold at the
// dial — would open holds the idle releaser can never close, because the releaser is driven by workspaces.
func (o *Orchestrator) holdMachine(ctx context.Context, tenant coordinator.Tenant, sessionID string, ch EngineChannel) (string, error) {
	machine := machineOf(ch)
	if machine == "" || sessionID == "" {
		return "", nil
	}
	occupancyID, err := o.spine.HoldMachine(ctx, tenant, sessionID, machine)
	switch {
	case errors.Is(err, coordinator.ErrMachineAtCapacity):
		log.Printf("session %s: machine %s already holds the capacity it declared; this run parks until a slot frees: %v",
			sessionID, machine, err)
		return "", err
	case errors.Is(err, coordinator.ErrMachineHeldByAnotherTenant):
		log.Printf("session %s: machine %s is held by another customer; this run parks until that hold settles: %v",
			sessionID, machine, err)
		return "", err
	case err != nil:
		log.Printf("session %s: machine %s is being occupied unmetered: %v", sessionID, machine, err)
		return "", nil
	}
	return occupancyID, nil
}

// keepMachineHeld moves the occupancy's last-activity stamp as an attempt ends.
//
// IT IS THE END OF THE ATTEMPT AND NOT ONLY THE START, and without it the bill is short by the length of
// every run. An idle-closed occupancy bills to `last_activity_at`, so a session whose only stamp was set
// when its run STARTED would have a two-hour run's machine time thrown away the moment the reaper closed
// it. Stamping at both ends makes the billed interval agree with what the idle sweep itself measures,
// which is `max(runs.updated_at)`.
//
// A touch on an occupancy the reaper already closed is a no-op by construction (see TouchLease): it must
// never move a stamp on a closed hold, because that would raise a bill that has already settled. So this
// racing an idle release is ordinary rather than exceptional, and needs no coordination here.
func (o *Orchestrator) keepMachineHeld(tenant coordinator.Tenant, occupancyID string) {
	if occupancyID == "" {
		return
	}
	// A fresh context for the reason releaseWorkspace uses one: this runs on every exit including a
	// cancelled one, and a dead ctx must not be what decides where a bill stops. Still tenant-scoped —
	// row-level security gates the update, so an unscoped write would silently touch nothing.
	ctx := storage.WithTenant(context.Background(), tenant.Project)
	if err := o.spine.TouchLease(ctx, tenant, occupancyID); err != nil {
		log.Printf("keep occupancy %s alive: %v", occupancyID, err)
	}
}
