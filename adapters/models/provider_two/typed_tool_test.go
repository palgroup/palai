package providertwo_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// captureToolsBody runs one request against a stub and returns the `tools` array the adapter built.
func captureToolsBody(t *testing.T, tools []modelbroker.ToolSchema) []map[string]any {
	t.Helper()
	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &captured)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedStream)
	}))
	defer srv.Close()

	req := toolSchemaRequest()
	req.ForceToolCall = false
	req.Tools = tools
	if _, err := (providertwo.Adapter{BaseURL: srv.URL}).Execute(context.Background(), req, sentinelSecret, nil); err != nil {
		t.Fatalf("execute: %v", err)
	}
	raw, ok := captured["tools"].([]any)
	if !ok {
		t.Fatalf("the request body carried no tools array: %v", captured)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestATypedToolIsSentAsTypeAndNameOnly is the shape the whole text-editor adoption rests on. An
// Anthropic-defined tool is DECLARED, not described: its type names a schema the model already
// carries, so sending an input_schema of ours would be a different request than the one the tool's
// behaviour was trained against.
func TestATypedToolIsSentAsTypeAndNameOnly(t *testing.T) {
	tools := captureToolsBody(t, []modelbroker.ToolSchema{
		{Name: "str_replace_based_edit_tool", Type: "text_editor_20250728"},
	})
	if len(tools) != 1 {
		t.Fatalf("sent %d tools, want 1", len(tools))
	}
	got := tools[0]
	if got["type"] != "text_editor_20250728" {
		t.Errorf("type = %v, want the Anthropic tool type", got["type"])
	}
	if _, present := got["input_schema"]; present {
		t.Error("input_schema is present on a typed tool; the key must be ABSENT, not empty")
	}
	if _, present := got["description"]; present {
		t.Error("description is present on a typed tool; the platform does not restate what it does")
	}
}

// TestTheEditorsNameSurvivesTheWireEncoding — `str_replace_based_edit_tool` is the literal the model
// was trained on. wireToolName rewrites anything outside [A-Za-z0-9_-] and truncates at 64, so this
// name happens to pass through untouched — but "happens to" is exactly what a guard is for, and a
// silent rewrite would be undetectable until a live run behaved oddly.
func TestTheEditorsNameSurvivesTheWireEncoding(t *testing.T) {
	tools := captureToolsBody(t, []modelbroker.ToolSchema{
		{Name: "str_replace_based_edit_tool", Type: "text_editor_20250728"},
	})
	if tools[0]["name"] != "str_replace_based_edit_tool" {
		t.Errorf("name on the wire = %v, want the exact trained literal", tools[0]["name"])
	}
}

// TestACustomToolIsUnchangedByTheTypedBranch pins today's shape for every tool this tree already
// ships. The branch must be invisible to them — no new key, no missing one.
func TestACustomToolIsUnchangedByTheTypedBranch(t *testing.T) {
	tools := captureToolsBody(t, []modelbroker.ToolSchema{{
		Name:        "palai.workspace.glob",
		Description: "find files by name",
		Parameters:  map[string]any{"type": "object"},
	}})
	got := tools[0]
	if got["name"] != "palai_workspace_glob" {
		t.Errorf("name = %v, want the sanitised wire name", got["name"])
	}
	if got["description"] != "find files by name" {
		t.Errorf("description = %v", got["description"])
	}
	if _, present := got["input_schema"]; !present {
		t.Error("input_schema is missing from a custom tool")
	}
	if _, present := got["type"]; present {
		t.Error("a custom tool gained a type key it never had")
	}
}

// TestATypedToolsSchemaNeverReachesTheWire replaces TestATypedToolCarryingASchemaIsRefused, and the
// swap is what keeps the property while dropping the wrong subject.
//
// That test asserted the combination was REFUSED, on the grounds that declaring both is a programming
// error. The property underneath it — an Anthropic request must not carry OUR schema for a tool whose
// shape the model already knows — is unchanged and is what is asserted here. What changed is that the
// combination stopped being an error: a typed tool that ALSO carries a schema is the PORTABLE one, and
// refusing it made every typed tool Anthropic-only. Measured 2026-08-06 on a live OpenAI-routed stack,
// that cost the editor entirely: `palai up` granted the canonical baseline containing it and routed the
// deployment to provider-one, and every run died at dispatch.
//
// So this adapter takes the type and DROPS the schema, which is the same wire it always sent — asserted
// here on the request body rather than on the refusal that used to stand in for it.
func TestATypedToolsSchemaNeverReachesTheWire(t *testing.T) {
	tools := captureToolsBody(t, []modelbroker.ToolSchema{{
		Name:        "str_replace_based_edit_tool",
		Type:        "text_editor_20250728",
		Description: "a description the other providers read",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}}},
	}})
	if len(tools) != 1 {
		t.Fatalf("sent %d tools, want 1", len(tools))
	}
	got := tools[0]
	if got["type"] != "text_editor_20250728" {
		t.Errorf("type = %v, want the Anthropic tool type", got["type"])
	}
	for _, key := range []string{"input_schema", "description"} {
		if _, present := got[key]; present {
			t.Errorf("the request carried %q for a TYPED tool (%v) — that is a different request shape "+
				"than the one this tool's behaviour was trained against, and the schema exists only for "+
				"providers that cannot carry a type", key, got[key])
		}
	}
}
