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
// to keep alive at attempt end. It returns "" when there is nothing to record — no machine on this
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
// A CAPACITY REFUSAL IS NOT ONE OF THOSE FAILURES AND IS LOGGED AS ITSELF (Faz A.4 T5). Every other error
// here means the metering could not be WRITTEN; ErrMachineAtCapacity means it was REFUSED — the machine is
// already holding as many sessions as `runners.capacity` allows, and this attempt is about to be the one
// over. Reporting it under the same sentence as a database blip would send the next reader to look for a
// database blip, which is the failure this tree keeps recording as an assertion pointing at the wrong file.
//
// A CAPACITY REFUSAL ENDS THE ATTEMPT RATHER THAN RUNNING IT UNMETERED, and the distinction from the
// paragraph above is the whole of this function (Faz A.4 T5). Running anyway would re-open exactly the hole
// T4 closed: a machine held, work done on it, and no row saying so. "The metering row would not write" and
// "this machine is not allowed to take this session" are opposite facts, and only one of them is the
// customer's benefit of the doubt.
//
// WHAT IT DOES NOT DO IS PARK THE RUN. A park is a promise that something will come, and nothing today
// wakes a run when a SLOT FREES: the only capacity wake is WakeRunAwaitingCapacity, fired from the
// gateway's handleConnect when a machine CONNECTS. A run parked for a full machine would wake on that
// machine's next connect, dial the same full machine, and park again — and because EnqueueWokenRunJob
// carries the budget already spent, the loop ends in a dead-letter rather than in a machine. The waker keyed
// on a settling occupancy is its own task; until it exists, the honest answer is an ended attempt with a
// reason, not a promise that cannot be kept.
//
// AND TODAY IT CANNOT FIRE AT ALL, which is why this is safe to ship ahead of that waker. No machine
// declares a capacity — the runner sends the field only when an operator configures one — so
// `runners.capacity` is 0 everywhere, the ceiling in leases.sql is not enforced, and every attempt takes
// its occupancy exactly as it did before. This branch exists for the deployment that opts in.
func (o *Orchestrator) holdMachine(ctx context.Context, tenant coordinator.Tenant, sessionID string, ch EngineChannel) (string, error) {
	machine := machineOf(ch)
	if machine == "" || sessionID == "" {
		return "", nil
	}
	occupancyID, err := o.spine.HoldMachine(ctx, tenant, sessionID, machine)
	switch {
	case errors.Is(err, coordinator.ErrMachineAtCapacity):
		log.Printf("session %s: machine %s already holds the capacity it declared; this attempt is refused rather than run unmetered: %v",
			sessionID, machine, err)
		return "", errMachineAtCapacity
	case err != nil:
		log.Printf("session %s: machine %s is being occupied unmetered: %v", sessionID, machine, err)
		return "", nil
	}
	return occupancyID, nil
}

// errMachineAtCapacity ends an attempt whose machine is already holding every session it declared it could.
// It is a FAILURE and not a park, for the reason holdMachine gives: a park needs a waker that fires when a
// slot frees, and the only capacity wake in this tree fires when a machine connects. The attempt rides the
// existing retry ladder, which is also what the gateway does today when every machine in a pool is leased.
var errMachineAtCapacity = errors.New("run_machine_at_capacity")

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
