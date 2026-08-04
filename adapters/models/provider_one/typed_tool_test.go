package providerone

import (
	"strings"
	"testing"

	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestATypedToolIsRefusedByName guards against the QUIET failure, which is the dangerous one here.
// This adapter speaks OpenAI's chat/completions shape, where an Anthropic-defined tool type has no
// meaning. Dropping such a tool silently would leave the model with no editor and no sign that
// anything was missing — the run would simply stop being able to change a line, and would look
// healthy doing it. Failing names the route as the thing to fix.
//
// It also covers adapters/models/openai_compatible, which EMBEDS this Adapter and therefore shares
// this body builder.
func TestATypedToolIsRefusedByName(t *testing.T) {
	_, _, err := buildBody(modelbroker.Request{
		Model:    "gpt-4o-mini",
		Messages: []modelbroker.Message{{Role: "user", Content: "edit the file"}},
		Tools: []modelbroker.ToolSchema{
			{Name: "str_replace_based_edit_tool", Type: "text_editor_20250728"},
		},
	})
	if err == nil {
		t.Fatal("a typed tool was accepted by an OpenAI-shaped adapter")
	}
	if !strings.Contains(err.Error(), "str_replace_based_edit_tool") {
		t.Errorf("error = %v, want it to name the tool", err)
	}
	if !strings.Contains(err.Error(), "text_editor_20250728") {
		t.Errorf("error = %v, want it to name the type that cannot cross", err)
	}
}

// TestCustomToolsAreUnaffectedByTheRefusal — every tool this tree ships today is a custom tool, and
// the refusal must be invisible to them.
func TestCustomToolsAreUnaffectedByTheRefusal(t *testing.T) {
	raw, _, err := buildBody(modelbroker.Request{
		Model:    "gpt-4o-mini",
		Messages: []modelbroker.Message{{Role: "user", Content: "find the file"}},
		Tools: []modelbroker.ToolSchema{
			{Name: "palai.workspace.glob", Description: "find files", Parameters: map[string]any{"type": "object"}},
		},
	})
	if err != nil {
		t.Fatalf("a custom tool was refused: %v", err)
	}
	if !strings.Contains(string(raw), "palai_workspace_glob") {
		t.Error("the custom tool did not reach the body")
	}
}
