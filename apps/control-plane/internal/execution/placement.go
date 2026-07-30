package execution

// PLACEMENT AND THE CAPACITY PARK, orchestrator side (E24 T4).
//
// This file is the PRODUCTION CALLER of fleet.ResolvePool, and that sentence is the reason it exists as
// its own file rather than as ten lines inside ExecuteAttempt. T2 built the resolution order — the run's
// own pool, the agent revision's binding, the project policy, the default — with a table-driven proof of
// each step, and NOTHING IN THE TREE CALLED IT. A placement rule nothing calls is a placement rule that
// does not happen: every run went to whichever machine the single queue handed over. This repository has
// shipped a fully-built, fully-tested, unreachable surface at least three times (E19 T9's
// CreateSlackConnection, E23's DecideToolApproval, E24 T3's own fenced call sites), so the wiring is
// named here and fenced by a test.

import (
	"errors"

	"context"

	"github.com/palgroup/palai/apps/control-plane/internal/fleet"
	"github.com/palgroup/palai/packages/coordinator"
)

// errRunAwaitingCapacity ends an attempt cleanly when the attempt's pool holds no machine of its tenant
// (§3.6 D12): the run is now WAITING, no engine process was ever opened, and the next machine to join
// that pool re-enters it through coordinator.WakeRunAwaitingCapacity. Like errRunPaused, errRunReleased
// and errRunParked it is NOT a failure — ExecuteAttempt returns nil on it, so the dispatch
// worker is freed even in a single-worker stack and a parked run costs no compute while nothing can run
// it.
var errRunAwaitingCapacity = errors.New("run_awaiting_capacity")

// place decides WHERE this attempt runs, carries the tenant onto the descriptor, and records the
// decision on the run.
//
// THE TENANT IS THE CHEAP HALF AND THE IMPORTANT ONE. It is already resolved — RunContext returned it
// and the context is scoped to it — so threading it onto the descriptor is one assignment; what it buys
// is that the gateway can refuse another tenant's machine at all, which before this it could not,
// because nothing on the runner plane carried a tenant (§3.6 D8).
//
// A DESCRIPTOR THAT ALREADY NAMES A POOL IS NOT OVERRIDDEN, and there is exactly one such caller shape
// today (a proof that dials a specific pool directly). Resolution is what the production dispatch path
// goes through, because ExecuteRun sets no pool at all.
func (o *Orchestrator) place(ctx context.Context, tenant coordinator.Tenant, attempt *AttemptDescriptor) error {
	attempt.Tenant = tenant
	inputs, err := o.spine.RunPlacement(ctx, tenant, string(attempt.RunID))
	if err != nil {
		return err
	}
	// The run's own timestamp, not this attempt's arrival: a run bounced by a cordon, a retry or a
	// resume keeps its place in its pool's queue instead of going to the back of the line each time.
	if attempt.QueuedAt.IsZero() {
		attempt.QueuedAt = inputs.QueuedAt
	}
	policy, err := o.spine.ProjectConfig(ctx, tenant)
	if err != nil {
		return err
	}
	if attempt.PoolID == "" {
		attempt.PoolID = fleet.ResolvePool(fleet.PoolRequest{
			RunPoolID: inputs.PoolID,
			// The agent revision's binding has nowhere to read from: agent_revisions carries no pool column
			// and T1's 000045 is the epic's only migration (FLT-P3). The step stays ORDERED and fed "" so
			// the next person to add the column does not have to rediscover which side of it it goes.
			RevisionPoolID:      "",
			PolicyPoolID:        policy.Pool,
			TenantDefaultPoolID: inputs.DefaultPoolID,
		})
	}
	// Written once. A run that already carries a pool is left alone by the statement itself, so a resume
	// returns to the SAME pool rather than being re-placed into another posture.
	if inputs.PoolID == "" {
		return o.spine.RecordRunPool(ctx, tenant, string(attempt.RunID), attempt.PoolID)
	}
	return nil
}

// parkForCapacity releases the run's compute because there is no machine to run it on, following E23
// T1's choreography and adding no second parking mechanism — two parking paths mean two waking bugs.
//
// IT CAPTURES NO CHECKPOINT, and that is a deliberate departure from parkRun, which takes one
// when a sink is wired. An approval parks at a boundary an engine REACHED; this parks at the dial,
// before any engine exists, so there is no boundary to capture and nothing that could offer one.
// Recovery is ladder rung 2 — the woken attempt replays the committed transcript — which is always
// available, and requiring a checkpoint here would make the park unavailable on every deployment
// without object storage, which is most of them.
func (o *Orchestrator) parkForCapacity(ctx context.Context, tenant coordinator.Tenant, attempt AttemptDescriptor) error {
	if err := o.spine.ParkRunForCapacity(ctx, tenant, string(attempt.RunID), string(attempt.AttemptID)); err != nil {
		return err
	}
	return errRunAwaitingCapacity
}
