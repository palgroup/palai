package execution

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// dispatchTool handles a tool.request: it runs the fenced, schema-checked tool through
// the broker, commits the completed tool_call row and its journal event, and only then
// delivers tool.result to the engine (commit-before-deliver, spec §24.7, §26.7). A
// redelivered tool_call_id replays the broker's cached result without re-executing.
func (o *Orchestrator) dispatchTool(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	callID, _ := frame.Data["tool_call_id"].(string)
	name, _ := frame.Data["name"].(string)
	args, _ := frame.Data["arguments"].(map[string]any)

	outcome, err := o.tools.Execute(ctx, contracts.ToolCallID(callID), name, args, st.attempt.Fence, o.execEnv(st))
	if err != nil {
		return fmt.Errorf("execute tool %q (%s): %w", name, callID, err)
	}
	st.usage = addUsage(st.usage, outcome.Usage)

	arguments, _ := json.Marshal(args)
	result, _ := json.Marshal(outcome.Result)
	payload, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "tool_call_id": callID})
	// The ledger row carries the tool's DECLARED replay class (copied at execute time) and the canonical
	// request hash, so a kill-after-execute row is classified and a duplicate tool_call_id is recognised
	// by content (spec §26.6, TOL-016). commit-before-deliver still holds.
	if _, err := o.spine.CommitToolResult(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID),
		st.attempt.Fence, callID, name, arguments, result, string(outcome.ReplayClass), outcome.Hash, toolCallCompletedEvent, payload); err != nil {
		return err
	}

	// The engine hands tool content back to the model as text, so serialize the
	// structured result to a JSON string rather than a nested object.
	data := map[string]any{"tool_call_id": callID, "content": string(result)}
	return st.ch.Send(ctx, o.frame(st, "tool.result", data, string(frame.ID)))
}

// execEnv is the per-attempt sandbox context the broker hands a workspace-touching tool: the
// allocation root every path confines to, whether this attempt holds a read-only snapshot, and the
// shell runner. A workspace-less attempt (no host path) yields a zero root, so a workspace tool
// fails cleanly instead of touching the control plane's own filesystem.
func (o *Orchestrator) execEnv(st *attemptState) toolbroker.ExecEnv {
	return toolbroker.ExecEnv{
		WorkspaceRoot: st.attempt.WorkspaceHostPath,
		ReadOnly:      st.attempt.WorkspaceReadOnly,
		Shell:         o.shell,
		Tasks:         o.tasks,
		Publications:  o.publications,
		Scope: toolbroker.TaskScope{
			Org: st.tenant.Organization, Project: st.tenant.Project,
			SessionID: st.sessionID, RunID: string(st.attempt.RunID), ResponseID: st.responseID,
		},
	}
}
