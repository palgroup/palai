package automation

import (
	"errors"
	"testing"
)

// TestUnknownRevisionFieldsRejected proves a revision body accepts ONLY the enforced executable-config
// subset (model/tools/instructions) and rejects everything else: the E12 fields (mcp/skills/hooks/
// knowledge) that arrive in a later epic, and — for a template — identity/delegation fields a template
// must never carry. Dead/unsupported config is never silently stored (honest naming, spec §2).
func TestUnknownRevisionFieldsRejected(t *testing.T) {
	// The enforced subset decodes cleanly.
	if _, err := DecodeRevisionInput([]byte(`{"model":"m","tools":["file"],"instructions":"go"}`)); err != nil {
		t.Fatalf("valid subset rejected: %v", err)
	}

	rejected := map[string]string{
		"E12 mcp field":           `{"model":"m","mcp":{"servers":[]}}`,
		"E12 skills field":        `{"model":"m","skills":["x"]}`,
		"E12 hooks field":         `{"model":"m","hooks":{"pre":"y"}}`,
		"E12 knowledge field":     `{"model":"m","knowledge":["kb"]}`,
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
