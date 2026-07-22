package execution

import (
	"context"

	"github.com/palgroup/palai/adapters/integrations/mcp"
	"github.com/palgroup/palai/packages/coordinator"
)

// mcpProgressSink journals an MCP tools/call's advisory progress notifications as tool_call.progress.v1
// events (E12 T5). It bridges the adapter's mcp.ProgressSink to the durable spine. A failed append is
// swallowed here: progress is advisory (spec §basic/utilities/progress), so it must never fail or stall the
// underlying tools/call.
type mcpProgressSink struct{ spine *coordinator.Store }

// NewMCPProgressSink wires the durable progress sink the MCP manager tags each tools/call progress
// notification through.
func NewMCPProgressSink(spine *coordinator.Store) mcp.ProgressSink {
	return mcpProgressSink{spine: spine}
}

func (s mcpProgressSink) ToolProgress(ctx context.Context, scope mcp.CallScope, p mcp.Progress) {
	_ = s.spine.AppendToolProgress(ctx,
		coordinator.Tenant{Organization: scope.Org, Project: scope.Project},
		scope.SessionID, scope.ResponseID, scope.CallID, p.Progress, p.Total, p.Message)
}
