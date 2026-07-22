package automation

import (
	"errors"
	"testing"
)

// TestAgentRevisionAcceptsExtensionFieldsNow proves the deliberate reversal of E11's unknown-field
// reject (phase-11 §7 devir 3 closure): the four E12 extension fields (tool_sets/mcp_connections/skills/
// hooks) now DECODE cleanly, so the wave-2 tasks (T5 mcp, T7 skills, T8 hooks) never have to touch this
// package again — T2 opens the schema for all four (the conflict shield). T2 itself only CONSUMES
// tool_sets; the other three ride opaque, validated by their owning task. Everything OUTSIDE the E12
// four is still rejected: knowledge (E17, same pattern later), template identity/delegation a template
// must never carry, and any arbitrary field — dead/unsupported config is never silently stored.
func TestAgentRevisionAcceptsExtensionFieldsNow(t *testing.T) {
	// The enforced executable subset still decodes cleanly.
	if _, err := DecodeRevisionInput([]byte(`{"model":"m","tools":["file"],"instructions":"go"}`)); err != nil {
		t.Fatalf("valid subset rejected: %v", err)
	}

	// The four E12 fields are now accepted (they were rejected in E11).
	accepted := map[string]string{
		"tool_sets":       `{"model":"m","tool_sets":["tsrev_1"]}`,
		"mcp_connections": `{"model":"m","mcp_connections":["mcpc_1"]}`,
		"skills":          `{"model":"m","skills":["skill_1"]}`,
		"hooks":           `{"model":"m","hooks":["hook_1"]}`,
	}
	for name, body := range accepted {
		if _, err := DecodeRevisionInput([]byte(body)); err != nil {
			t.Errorf("%s: err = %v, want the E12 field accepted now", name, err)
		}
	}
	// tool_sets is consumed by T2: it surfaces on the decoded input.
	in, err := DecodeRevisionInput([]byte(`{"model":"m","tool_sets":["tsrev_a","tsrev_b"]}`))
	if err != nil {
		t.Fatalf("decode tool_sets: %v", err)
	}
	if len(in.ToolSets) != 2 || in.ToolSets[0] != "tsrev_a" {
		t.Fatalf("decoded tool_sets = %v, want [tsrev_a tsrev_b]", in.ToolSets)
	}

	// Everything outside the E12 four is still rejected.
	rejected := map[string]string{
		"E17 knowledge field":     `{"model":"m","knowledge":["kb"]}`,
		"template delegation":     `{"model":"m","delegation":{"emit":["child"]}}`,
		"template identity":       `{"model":"m","identity":"agent-x"}`,
		"unknown arbitrary field": `{"model":"m","surprise":true}`,
	}
	for name, body := range rejected {
		if _, err := DecodeRevisionInput([]byte(body)); !errors.Is(err, ErrUnknownField) {
			t.Errorf("%s: err = %v, want ErrUnknownField (the field must be rejected, not stored)", name, err)
		}
	}
}

// TestRevisionInputToolsCeilingSemantics pins the nil-vs-empty tools distinction the resolver relies on:
// nil = no ceiling, a non-nil (even empty) set = a ceiling. marshalTools keeps nil as SQL NULL.
func TestRevisionInputToolsCeilingSemantics(t *testing.T) {
	in, err := DecodeRevisionInput([]byte(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if in.Tools != nil {
		t.Fatalf("absent tools = %v, want nil (no ceiling)", in.Tools)
	}
	if marshalTools(in.Tools) != nil {
		t.Fatal("nil tools must marshal to a SQL NULL (no ceiling), not an empty array")
	}
	empty, _ := DecodeRevisionInput([]byte(`{"model":"m","tools":[]}`))
	if empty.Tools == nil {
		t.Fatal("an explicit [] tools set is a ceiling (allow nothing), not nil")
	}
}
