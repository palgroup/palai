package tools

import "testing"

// TestTheShellSchemaDeclaresItsTypes is the first consumer of the validator's array/boolean support,
// and it exists because of what shipped without it: `argv` carried the sentence "command and
// arguments as a JSON array of strings" and NO type, so the only thing between the model and a
// malformed call was English prose. The validator could not have been given the type either — it
// ERRORED on `array` until it was taught the case, so an untyped field was the only shape that
// worked.
func TestTheShellSchemaDeclaresItsTypes(t *testing.T) {
	props, ok := ShellTool().InputSchema["properties"].(map[string]any)
	if !ok {
		t.Fatal("the shell tool's schema has no properties map")
	}
	for name, wantType := range map[string]string{
		"argv":       "array",
		"shell":      "boolean",
		"background": "boolean",
	} {
		field, present := props[name].(map[string]any)
		if !present {
			t.Errorf("property %q is missing", name)
			continue
		}
		if got, _ := field["type"].(string); got != wantType {
			t.Errorf("property %q declares type %q, want %q — an untyped field is checked by nothing", name, got, wantType)
		}
	}
	argv, _ := props["argv"].(map[string]any)
	items, ok := argv["items"].(map[string]any)
	if !ok || items["type"] != "string" {
		t.Error(`argv declares no {"type":"string"} items, so a call carrying [1,2] would validate`)
	}
}

// TestTheShellSchemaKeepsItsDescriptions pins the half of each property that was already right. The
// descriptions carry measured behaviour — what `background` returns, which tool reads its output —
// and adding a type is not licence to reword them.
func TestTheShellSchemaKeepsItsDescriptions(t *testing.T) {
	props, _ := ShellTool().InputSchema["properties"].(map[string]any)
	for _, name := range []string{"argv", "shell", "background"} {
		field, _ := props[name].(map[string]any)
		if desc, _ := field["description"].(string); desc == "" {
			t.Errorf("property %q lost its description", name)
		}
	}
}
