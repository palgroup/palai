package execution

// WHICH MACHINE, from inside the pool (Faz A.5 T6).
//
// placement.go decides WHICH POOL an attempt waits on and stops there. Which MACHINE in that pool serves it
// was, until this file, whichever one happened to sit at the head of the queue's parked list: nothing in the
// tree compared one machine to another at all. The claim is recorded as a command that STAYS checkable
// rather than as the one it was first measured with — that one grepped for "least" and "emptiest", words
// this file now contains, so it would answer differently forever after and prove nothing to the next reader:
//
//	grep -rln 'emptiestFirst\|machineWeight\|PoolMachineLoads' --include='*.go' . \
//	  | grep -v node_modules | grep -v _test
//	-> 3 (2026-08-05): this file, the Dial that consults it, and the store read they share. It was 0.
//
// That was invisible for as long as a Mac pool held one machine, and it is the owner's sentence that makes
// it matter: "ten Macs, a session request arrives, open the session on the EMPTIEST Mac". With one machine
// per pool the head of the list is the emptiest by construction. With ten, a Mac carrying three idle
// allocations and a Mac carrying none were the same candidate.
//
// WHAT MADE THIS POSSIBLE IS 4f0c3703, and without it this file would be a way to break things. A session's
// workspace now FOLLOWS it between Macs (or the run refuses loudly), so every machine in a pool can really
// take every session and the choice is genuinely free. Before that, choosing a different Mac silently meant
// running against an empty directory.
//
// THE NUMBER IS OPEN OCCUPANCIES AND NOT IN-FLIGHT LEASES, and the two are far apart on exactly the fleet
// this exists for. `runnerLifecycle.active` counts the leases this gateway is serving right now, so a
// PARKED machine reads 0 no matter how many allocations it is holding — and an allocation is precisely what
// survives between messages, which is what makes a Mac full. The number that decides admission is
// `runner_leases` with `released_at IS NULL`, so that is the number the preference reads: ranking by a
// different one would send every run to the machine most likely to refuse it.
//
// "EMPTIEST" IS FEWEST OPEN HOLDS AND NOT MOST FREE SLOTS, which is a choice and not the only one. Most
// free slots is UNDEFINED for the machines every shipped deployment actually has: `capacity = 0` means the
// machine declared no ceiling, so it has no slot count to subtract from, and a fleet of undeclared machines
// would be ranked by a number that does not exist. Fewest open holds is defined for every machine, declared
// or not.
//
// AND IT IS A PREFERENCE, NEVER A REFUSAL. Everything below can be stale by the time it is acted on, so
// nothing below is allowed to be the reason a hold does not open — `coordinator.AcquireLease` is, from
// inside the statement that writes the row. A machine that looks full here is still handed the run when it
// is the best of what is parked, because the snapshot goes stale in BOTH directions and a hold may have
// settled since. A pre-check that refused here would be a second ceiling, weaker than the real one and
// disagreeing with it under contention, which is the shape orchestrator.go names and turns down.

import (
	"context"
	"log"

	"github.com/palgroup/palai/packages/coordinator"
)

// MachineLoadView weighs one pool's machines for a placement. *coordinator.Store is the only
// implementation; the interface exists so the gateway does not depend on the database, exactly as it does
// not depend on it for the registry or for the capacity wake.
type MachineLoadView interface {
	// PoolMachineLoads reports every machine in the pool with its open holds and the ceiling it declared.
	// A machine it does not answer for is one the caller knows nothing about — see emptiestFirst for what
	// the ranking does with that, which is deliberately NOT "assume the worst".
	PoolMachineLoads(ctx context.Context, tenant coordinator.Tenant, poolID string) ([]coordinator.PoolMachineLoad, error)
}

// SetMachineLoadView wires the placement preference. Unset, Dial takes the machine at the head of its
// pool's parked list exactly as it did before this file existed — the posture of every Docker-free wire
// proof and of the conformance tier, where there is no database to weigh anything with.
func (g *RunnerGateway) SetMachineLoadView(v MachineLoadView) { g.loads = v }

// machineWeight is one machine's standing in a handover: whether it is already at the ceiling it declared,
// and how many holds are open on it.
//
// `full` IS A RANK AND NOT A FILTER. A machine that looks full is ranked BELOW every machine with room and
// is still handed the run when it is all there is — see the file header for why refusing here would be a
// second ceiling.
type machineWeight struct {
	full bool
	open int
}

// better reports whether this machine should be handed the run ahead of the one currently winning.
//
// ROOM FIRST, THEN EMPTINESS, and the order matters for a mixed fleet: a machine that declared 1 and holds
// 1 has fewer open holds than a machine that declared 10 and holds 3, and handing it the run would be
// choosing the one machine present that cannot take it.
//
// THE COMPARISONS ARE STRICT, WHICH IS WHAT NAMES THE TIE-BREAK. Two equally empty machines compare false
// here, so the one already winning keeps the place — and since the scan walks the parked list in order,
// that is the machine that has been parked longest. Without a named tie-break two equal machines would be
// decided by map iteration and the proof of this file would be a coin toss.
func (w machineWeight) better(than machineWeight) bool {
	if w.full != than.full {
		return !w.full
	}
	return w.open < than.open
}

// machinePreference weighs one parked machine by its server-minted runner id. nil means "no preference" and
// every caller reads it as today's behaviour: take the head of the parked list.
type machinePreference func(runnerID string) machineWeight

// emptiestFirst builds the preference for ONE handover, reading the pool's loads once, BEFORE the queue's
// lock is taken.
//
// THAT ORDERING IS THE WHOLE REASON THIS IS A FUNCTION AND NOT A LOOKUP INSIDE THE QUEUE. runnerLifecycle
// (runner_gateway.go) turns a per-machine comparison inside the handover down for two reasons, and both are
// answered rather than ignored. The first — that an ELIGIBILITY rule must be expressed by removing the
// machine from the rendezvous, so no code path is left that a later change could soften into a preference —
// does not reach this, because this IS a preference: a rule that ranks cannot be expressed by removal at
// all, since removing every machine but one would make an idle fleet unreachable the moment the preferred
// machine went away. The second — that the queue must not consult per-runner state under its own lock — is
// honoured exactly: the read happens here, the queue is handed a finished function, and the map it closes
// over is never written again.
//
// IT ANSWERS nil WHENEVER THERE IS NOTHING TO CHOOSE, and each case is a real posture rather than a
// defensive habit:
//
//   - no view wired: a gateway with no durable spine, which is every wire proof and the conformance tier.
//   - no tenant: the pre-E24 attempt, whose rendezvous holds only machines no registry knows.
//   - fewer than two distinct machines parked: there is no choice to make, so the query is not issued at
//     all and the path is byte-identical to the one before this file.
//   - the read failed: a placement preference is not worth failing a dial over. The run takes the head of
//     the list, which is what it would have taken yesterday, and the log line is the record.
//
// The machines are counted DISTINCT because one machine parks one session per concurrent lease
// (PALAI_RUNNER_CONCURRENCY), so a single Mac offering three loops is three parked entries and still no
// choice at all.
func (g *RunnerGateway) emptiestFirst(ctx context.Context, tenant coordinator.Tenant, poolID string, parked []string) machinePreference {
	if g.loads == nil || tenant.Project == "" || len(parked) < 2 {
		return nil
	}
	pool := poolKey(poolID)
	loads, err := g.loads.PoolMachineLoads(ctx, tenant, pool)
	if err != nil {
		log.Printf("pool %s: weighing its machines failed, so this run takes the machine at the head of the queue: %v", pool, err)
		return nil
	}
	weights := make(map[string]machineWeight, len(loads))
	for _, load := range loads {
		// `capacity = 0` MEANS DECLARED NOTHING, THEREFORE NO CEILING, THEREFORE ALWAYS ROOM — and it is
		// ASKED (coordinator.PoolMachineLoad.HasRoom) rather than restated here, because it is the acquire's
		// own guard and a second copy is how the two come to disagree. Written as a bare
		// `open >= capacity`, a machine that simply never said anything would rank as permanently full and
		// sort below every declared machine; since no machine in any shipped deployment declares a capacity,
		// that inversion would rank the ENTIRE fleet as full and make this file's ordering meaningless
		// everywhere it actually runs.
		weights[load.RunnerID] = machineWeight{full: !load.HasRoom(), open: load.Open}
	}
	// A machine the view did not answer for gets the zero weight — not full, no open holds. NOT PENALISED,
	// which is the deliberate half: absence means the read knows nothing about it (a machine enrolled
	// against no registry, a row this tenant cannot see), and a stack where nothing is known ranks every
	// machine equal and falls back to park order, which is exactly the behaviour that shipped before this.
	return func(runnerID string) machineWeight { return weights[runnerID] }
}
