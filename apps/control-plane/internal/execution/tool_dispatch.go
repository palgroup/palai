package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
)

// dispatchTool handles a tool.request: it runs the fenced, schema-checked tool through
// the broker, commits the completed tool_call row and its journal event, and only then
// delivers tool.result to the engine (commit-before-deliver, spec §24.7, §26.7). A
// redelivered tool_call_id replays the broker's cached result without re-executing.
func (o *Orchestrator) dispatchTool(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	callID, _ := frame.Data["tool_call_id"].(string)
	name, _ := frame.Data["name"].(string)
	args, _ := frame.Data["arguments"].(map[string]any)

	outcome, err := o.tools.Execute(contracts.ToolCallID(callID), name, args, st.attempt.Fence)
	if err != nil {
		return fmt.Errorf("execute tool %q (%s): %w", name, callID, err)
	}
	st.usage = addUsage(st.usage, outcome.Usage)

	arguments, _ := json.Marshal(args)
	result, _ := json.Marshal(outcome.Result)
	payload, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "tool_call_id": callID})
	if _, err := o.spine.CommitToolResult(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID),
		st.attempt.Fence, callID, name, arguments, result, toolCallCompletedEvent, payload); err != nil {
		return err
	}

	// The engine hands tool content back to the model as text, so serialize the
	// structured result to a JSON string rather than a nested object.
	data := map[string]any{"tool_call_id": callID, "content": string(result)}
	return st.ch.Send(ctx, o.frame(st, "tool.result", data, string(frame.ID)))
}
