package toolbroker

import "testing"

// TestValidateAcceptsArrays covers the type `argv` has needed since the shell tool was written: a
// JSON array of strings. The validator ERRORED on it — `default: unsupported schema type` — so the
// field shipped with no type at all and the model learned its shape from an English sentence.
func TestValidateAcceptsArrays(t *testing.T) {
	schema := map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
	if err := validate(schema, []any{"go", "test", "./..."}); err != nil {
		t.Fatalf("an array of strings was rejected: %v", err)
	}
	if err := validate(schema, []any{"go", 7}); err == nil {
		t.Error("an array carrying a non-string was accepted; `items` must be enforced")
	}
	if err := validate(schema, "not an array"); err == nil {
		t.Error("a string was accepted where an array was declared")
	}
	// `items` is OPTIONAL: an array schema without it constrains the container only, which is the
	// same thing an untyped schema does for a scalar.
	if err := validate(map[string]any{"type": "array"}, []any{1, "two", true}); err != nil {
		t.Errorf("an itemless array schema rejected a mixed array: %v", err)
	}
}

// TestValidateAcceptsBooleans — `shell` and `background` are both booleans that shipped untyped, so a
// model sending the STRING "true" was accepted and reached the tool as a non-bool.
func TestValidateAcceptsBooleans(t *testing.T) {
	schema := map[string]any{"type": "boolean"}
	for _, ok := range []any{true, false} {
		if err := validate(schema, ok); err != nil {
			t.Errorf("%v was rejected: %v", ok, err)
		}
	}
	for _, bad := range []any{"true", 1, nil} {
		if err := validate(schema, bad); err == nil {
			t.Errorf("%#v was accepted where a boolean was declared", bad)
		}
	}
}

// TestValidateEnforcesEnum is what makes an output_mode declarable. An enum constrains the VALUE and
// is independent of `type`, so it is checked whether or not a type is present. A near-miss
// ("contents" for "content") is exactly the failure it exists to catch, and exactly the one a
// description sentence never catches.
func TestValidateEnforcesEnum(t *testing.T) {
	schema := map[string]any{"type": "string", "enum": []any{"content", "files_with_matches", "count"}}
	if err := validate(schema, "content"); err != nil {
		t.Fatalf("a listed value was rejected: %v", err)
	}
	if err := validate(schema, "contents"); err == nil {
		t.Error("an unlisted near-miss was accepted")
	}
	if err := validate(map[string]any{"enum": []any{1.0, 2.0}}, 3.0); err == nil {
		t.Error("an unlisted value was accepted on a typeless enum schema")
	}
	if err := validate(map[string]any{"enum": []any{1.0, 2.0}}, 2.0); err != nil {
		t.Errorf("a listed value was rejected on a typeless enum schema: %v", err)
	}
}

// TestValidateStillRejectsAnUnknownType pins the property the default arm carries: this validator
// REFUSES what it does not understand rather than waving it through. A tool declaring a type outside
// this subset must fail loudly rather than ship an unchecked field — which is the whole reason the
// three cases above had to be added rather than worked around.
func TestValidateStillRejectsAnUnknownType(t *testing.T) {
	if err := validate(map[string]any{"type": "null"}, nil); err == nil {
		t.Fatal("an unsupported type was accepted; the rejecting default is load-bearing")
	}
}

// TestValidateStillEnforcesTheObjectRules guards what already worked, so the new cases cannot be
// added by loosening the old ones.
func TestValidateStillEnforcesTheObjectRules(t *testing.T) {
	schema := map[string]any{
		"type":                 "object",
		"properties":           map[string]any{"a": map[string]any{"type": "string"}},
		"required":             []any{"a"},
		"additionalProperties": false,
	}
	if err := validate(schema, map[string]any{"a": "ok"}); err != nil {
		t.Fatalf("a valid object was rejected: %v", err)
	}
	if err := validate(schema, map[string]any{}); err == nil {
		t.Error("a missing required property was accepted")
	}
	if err := validate(schema, map[string]any{"a": "ok", "b": 1}); err == nil {
		t.Error("an undeclared property was accepted under additionalProperties:false")
	}
	if err := validate(schema, map[string]any{"a": 1}); err == nil {
		t.Error("a wrongly-typed property was accepted")
	}
}
