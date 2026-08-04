package execution

import (
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestTheToolTypeReachesTheWire guards the failure this tree names most often: a field added at both
// ends and dropped in the middle. An Anthropic-defined tool is IDENTIFIED by its type — the schema it
// works to lives in the model, not in InputSchema — so a conversion that loses the type turns the tool
// into an unknown custom tool with no schema at all, and the model is offered something it has never
// seen. Nothing about that failure is visible from either struct definition.
func TestTheToolTypeReachesTheWire(t *testing.T) {
	typed := toolbroker.Tool{
		Name: "str_replace_based_edit_tool",
		Type: "text_editor_20250728",
	}
	got := toolSchemaFor(typed)
	if got.Type != "text_editor_20250728" {
		t.Errorf("Type = %q, want it carried across the seam", got.Type)
	}
	if got.Name != "str_replace_based_edit_tool" {
		t.Errorf("Name = %q", got.Name)
	}
	if len(got.Parameters) != 0 {
		t.Errorf("Parameters = %v, want empty: a typed tool carries no schema of ours", got.Parameters)
	}
}

// TestAnOrdinaryToolCrossesUnchanged pins the other half. Every tool this tree ships today is a custom
// tool, and this change must not perturb the shape they arrive in — an empty Type must stay empty
// rather than becoming a key the provider has to ignore.
func TestAnOrdinaryToolCrossesUnchanged(t *testing.T) {
	custom := toolbroker.Tool{
		Name:        "palai.workspace.glob",
		Description: "find files by name",
		InputSchema: map[string]any{"type": "object"},
	}
	got := toolSchemaFor(custom)
	if got.Type != "" {
		t.Errorf("Type = %q on a custom tool, want empty", got.Type)
	}
	if got.Name != custom.Name || got.Description != custom.Description {
		t.Errorf("name/description changed: %+v", got)
	}
	if len(got.Parameters) != 1 {
		t.Errorf("Parameters = %v, want the tool's own schema", got.Parameters)
	}
}
