package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// terminalCommands maps an engine run.terminal outcome to its canonical run command
// and the terminal response status (spec §25.8, §22.3).
var terminalCommands = map[string]struct {
	command statemachines.RunCommand
	status  string
}{
	"completed":       {statemachines.RunCmdComplete, "completed"},
	"failed":          {statemachines.RunCmdFail, "failed"},
	"canceled":        {statemachines.RunCmdCancel, "canceled"},
	"timed_out":       {statemachines.RunCmdTimeout, "timed_out"},
	"budget_exceeded": {statemachines.RunCmdExhaustBudget, "budget_exceeded"},
}

// finalize handles run.terminal: it applies exactly one terminal run transition and
// writes the terminal Response projection from the committed run, output, and usage.
func (o *Orchestrator) finalize(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	outcome, _ := frame.Data["outcome"].(string)
	terminal, ok := terminalCommands[outcome]
	if !ok {
		return fmt.Errorf("engine terminal frame has unknown outcome %q", outcome)
	}

	// Exactly one terminal transition. A run already terminal (idempotent redelivery)
	// is not re-transitioned, but its projection is still refreshed below.
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), terminal.command); {
	case errors.Is(err, coordinator.ErrRunTerminal), errors.Is(err, statemachines.ErrInvalidState):
	case err != nil:
		return err
	}

	output := st.output
	if len(output) == 0 {
		if value, ok := frame.Data["output"]; ok && value != nil {
			output = []contracts.ContentItem{{"type": "message", "content": value}}
		}
	}
	projection, err := json.Marshal(map[string]any{"output": output, "usage": st.usage})
	if err != nil {
		return fmt.Errorf("marshal response projection: %w", err)
	}
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, terminal.status, projection)
}
