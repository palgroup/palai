package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// RunAdvancer is the transactional run-transition seam a job handler drives runs
// through. *coordinator.Store implements it (its ApplyRunTransition locks the run,
// applies the pure state machine, and commits state + event + outbox in one tx); a
// fake implements it in unit tests.
type RunAdvancer interface {
	ApplyRunTransition(ctx context.Context, tenant coordinator.Tenant, runID string, cmd statemachines.RunCommand) (coordinator.Transition, error)
}

// runJobPayload is the durable job body for a response.run job: the run it assigns.
type runJobPayload struct {
	RunID string `json:"run_id"`
}

// assignmentPlan moves a queued run into execution. No engine runs yet (spec §24.4),
// so assignment drives the run to running and leaves completion to the engine in a
// later task.
var assignmentPlan = []statemachines.RunCommand{
	statemachines.RunCmdProvision,
	statemachines.RunCmdStart,
}

// AdvanceRun is the coordinator Handler that turns a claimed response.run job into
// durable run assignment: it drives the referenced run queued -> provisioning ->
// running through ApplyRunTransition, which emits the journal events the SSE layer
// already serves. It is idempotent under redelivery — a transition already applied by
// an earlier attempt (for example after the previous worker was killed mid-assign) is
// skipped rather than errored, so a reclaimed job resumes instead of failing. The
// returned result hash records the assigned run for the authoritative completion.
func AdvanceRun(advancer RunAdvancer) coordinator.Handler {
	return func(ctx context.Context, claim coordinator.Claim, payload []byte) (string, error) {
		var body runJobPayload
		if err := json.Unmarshal(payload, &body); err != nil {
			return "", fmt.Errorf("decode run job payload: %w", err)
		}
		if body.RunID == "" {
			return "", errors.New("run job payload is missing run_id")
		}
		for _, cmd := range assignmentPlan {
			_, err := advancer.ApplyRunTransition(ctx, claim.Tenant, body.RunID, cmd)
			if errors.Is(err, statemachines.ErrInvalidState) {
				continue // already applied by an earlier attempt; resume idempotently
			}
			if err != nil {
				return "", fmt.Errorf("advance run %s via %s: %w", body.RunID, cmd, err)
			}
		}
		return "run:" + body.RunID + ":assigned", nil
	}
}
