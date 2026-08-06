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

// TestATypedToolWithASchemaCrossesAsAnOrdinaryFunction is the half the refusal above cost until
// 2026-08-06, and it is the whole reason an OpenAI-routed deployment has an editor at all.
//
// The refusal was right about a tool that carries ONLY a type: there is nothing to advertise it with.
// It was wrong as a blanket rule, and the cost was measured on a live stack — `palai up` grants the
// canonical tool baseline, which contains str_replace_based_edit_tool, AND routes the deployment to
// provider-one when PALAI_MODEL_PROVIDER says so. Those two halves of ONE bring-up were mutually
// exclusive: every run died at dispatch with "route this agent to an Anthropic model or drop the tool
// from its set" — advice nobody could act on, because the stack's own bring-up had chosen both. An agent
// that cannot reach the editor cannot change a line of code.
//
// The exec is identical either way; only the advertisement differs. So a typed tool that ALSO carries a
// schema crosses here as a plain function, and provider_two still sends {type, name} and drops the
// schema (its own test asserts the wire).
func TestATypedToolWithASchemaCrossesAsAnOrdinaryFunction(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"command": map[string]any{"type": "string"}},
	}
	raw, _, err := buildBody(modelbroker.Request{
		Model:    "gpt-4o-mini",
		Messages: []modelbroker.Message{{Role: "user", Content: "edit the file"}},
		Tools: []modelbroker.ToolSchema{{
			Name:        "str_replace_based_edit_tool",
			Type:        "text_editor_20250728",
			Description: "view and edit files",
			Parameters:  schema,
		}},
	})
	if err != nil {
		t.Fatalf("a typed tool carrying a schema was refused: %v — an OpenAI-routed deployment has no "+
			"editor, so its agents cannot change a line of code", err)
	}
	body := string(raw)
	for _, want := range []string{`"type":"function"`, `"str_replace_based_edit_tool"`, `"view and edit files"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the request body does not carry %s: %s", want, body)
		}
	}
	// The Anthropic type must NOT be sent: this provider has no meaning for it, and a request carrying
	// an unknown tool type is a request the provider may reject wholesale.
	if strings.Contains(body, "text_editor_20250728") {
		t.Errorf("the OpenAI request carries the Anthropic tool type: %s", body)
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
