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

// errPoolNotServable ends an attempt whose resolved pool is not one this tenant can be served by, and
// it exists because the alternative shipped and produced runs nothing could ever move.
//
// `fleet.ResolvePool`'s last resort is the CONSTANT `pool_default`, which belongs to the BOOTSTRAP
// tenant. Every other tenant that has configured nothing resolves to it, `RecordRunPool` correctly
// refuses to record another tenant's pool, and `runs.pool_id` stays NULL — while the dial went ahead
// and PARKED the run. The park's own wake, `OldestRunAwaitingCapacity`, matches on `r.pool_id = $3`;
// NULL matches nothing. So the run was `waiting` with an `awaiting_capacity` attempt and no machine
// joining any pool could reach it, no reaper covered it (`SweepExpiredCapacityParks` is a no-op unless
// PALAI_FLEET_PARK_TTL is set, and the shipped default is unset), and no error reached the caller. Two
// runs on a live stack sat exactly like that for thirty-one hours.
//
// It is a FAILURE and not a park on purpose. A park is a promise that something will come; nothing
// will come to a pool this tenant has no machine in and cannot enrol one into. The honest answer is a
// terminal the caller can read and an operator can act on — create a pool for the project, or name one
// in its config_policy — which is the same stance failProvisioning takes for a bad clone URL.
var errPoolNotServable = errors.New("run_pool_not_servable")

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
	//
	// AND THE WRITE IS CHECKED, which is the 2026-08-02 fix. RecordRunPool records nothing for a pool
	// this tenant cannot be served by, and until now that was silent: the attempt dialled into a pool it
	// had not recorded, parked there, and left a run its own wake could not find (errPoolNotServable).
	// The recorded id is carried onto the descriptor rather than the resolved one, so an attempt that
	// lost a race to record dials where the DURABLE decision says, not where its own resolution said.
	if inputs.PoolID == "" {
		recorded, err := o.spine.RecordRunPool(ctx, tenant, string(attempt.RunID), attempt.PoolID)
		switch {
		case errors.Is(err, coordinator.ErrRunPoolNotRecordable):
			return errPoolNotServable
		case err != nil:
			return err
		}
		attempt.PoolID = recorded
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
