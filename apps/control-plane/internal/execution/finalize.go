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

// problemTypePrefix namespaces stable codes into dereferenceable problem types,
// matching the HTTP surface's middleware.WriteProblem so a stored terminal error and a
// live problem document share one type URI.
const problemTypePrefix = "https://docs.palai.dev/problems/"

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

// terminalProblems maps a non-completed terminal status to the sanitized RFC 9457
// problem the Response projection carries as its error (spec §22.3, §8.3). Each detail
// is a fixed human line — never raw provider or engine text. request_id is stamped at
// retrieval, not here, since a terminal is finalized off any HTTP request.
var terminalProblems = map[string]contracts.Problem{
	"failed":          {Code: "internal_error", Title: "Internal error", Status: 500, Detail: "the run failed during execution", Retryable: true},
	"timed_out":       {Code: "operation_timed_out", Title: "Operation timed out", Status: 504, Detail: "the run exceeded its execution deadline", Retryable: true},
	"budget_exceeded": {Code: "quota_exceeded", Title: "Quota exceeded", Status: 429, Detail: "the run exhausted its allotted budget"},
	"canceled":        {Code: "canceled", Title: "Canceled", Status: 409, Detail: "the run was canceled before completion"},
}

// terminalProblem returns the sanitized problem a non-completed terminal projects as
// its error, or nil for a completed run (which carries no error). The type URI is
// derived from the stable code so it stays consistent with the HTTP surface.
func terminalProblem(status string) *contracts.Problem {
	p, ok := terminalProblems[status]
	if !ok {
		return nil
	}
	p.Type = problemTypePrefix + p.Code
	return &p
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
	proj := map[string]any{"output": output, "usage": st.usage, "model": st.model}
	if problem := terminalProblem(terminal.status); problem != nil {
		proj["error"] = problem
	}
	projection, err := json.Marshal(proj)
	if err != nil {
		return fmt.Errorf("marshal response projection: %w", err)
	}
	return o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, terminal.status, projection)
}
