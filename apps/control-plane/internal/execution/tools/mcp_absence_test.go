package tools

import (
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestNoModelFacingMCPManagementTool pins the E12 T5 admin-only posture: the built-in tool set a model can
// call directly carries NO tool that adds, discovers, or configures an MCP connection. MCP registration +
// discovery are admin API actions (/v1/mcp-connections[/discover]); the model can only CALL an already
// admin-approved, published, pinned discovered tool — never create or discover one. A future built-in whose
// name reads as MCP management fails here rather than quietly widening the model's authority.
func TestNoModelFacingMCPManagementTool(t *testing.T) {
	builtins := toolbroker.New(
		FileTool(), ShellTool(), CommitTool(), PushTool(), PullRequestTool(), ResearchFetchTool(),
	)
	for _, name := range builtins.Names() {
		lower := strings.ToLower(name)
		for _, forbidden := range []string{"mcp", "connection", "discover"} {
			if strings.Contains(lower, forbidden) {
				t.Errorf("built-in tool %q looks like MCP management (%q) — MCP add/discover must be admin-API-only", name, forbidden)
			}
		}
	}
}
