package outputcontract

import (
	"errors"
	"strings"
	"testing"
)

func mustParse(t *testing.T, raw map[string]any) Contract {
	t.Helper()
	c, err := Parse(raw)
	if err != nil {
		t.Fatalf("Parse(%v) error = %v, want nil", raw, err)
	}
	return c
}

// TestAbsentOutputIsTheTextDefault is the compatibility fence. Every request ever sent to this
// server omitted `output` or sent a string `format`, and none of them may change meaning.
func TestAbsentOutputIsTheTextDefault(t *testing.T) {
	for _, raw := range []map[string]any{nil, {}} {
		c := mustParse(t, raw)
		if c.Format != FormatText || c.Demanded() {
			t.Fatalf("Parse(%v) = %+v, want the text default with nothing demanded", raw, c)
		}
	}
}

// TestPreviouslyPublishedTextFormatStillMeansText pins the exact promise made in the schema's own
// description: `format` stays a STRING and `"text"` keeps its meaning and its behaviour.
func TestPreviouslyPublishedTextFormatStillMeansText(t *testing.T) {
	c := mustParse(t, map[string]any{"format": "text"})
	if c.Format != FormatText || c.Demanded() {
		t.Fatalf(`Parse({"format":"text"}) = %+v, want text with nothing demanded`, c)
	}
}

// TestUnsupportedFormatIsRefusedRatherThanDropped is the ONE behaviour change, asserted on purpose
// so it can never happen silently again. Before 2026-08-01 every value here was accepted and dropped.
func TestUnsupportedFormatIsRefusedRatherThanDropped(t *testing.T) {
	_, err := Parse(map[string]any{"format": "yaml"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf(`Parse({"format":"yaml"}) error = %v, want ErrInvalid`, err)
	}
	if !strings.Contains(err.Error(), "yaml") {
		t.Fatalf("refusal %q does not name the value the caller sent", err)
	}
}

// TestMisspelledKeyIsRefused is the defect in miniature: a caller writes `schemas`, the server drops
// it, and the run answers prose while the caller believes a contract is in force.
func TestMisspelledKeyIsRefused(t *testing.T) {
	_, err := Parse(map[string]any{"format": "json_schema", "schemas": map[string]any{"type": "object"}})
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "schemas") {
		t.Fatalf("Parse with a misspelled key error = %v, want a refusal naming %q", err, "schemas")
	}
}

// TestSchemaWithoutJSONSchemaFormatIsRefused catches the other half of the same confusion: a schema
// supplied under the text default would never be enforced, so accepting it would be a silent lie.
func TestSchemaWithoutJSONSchemaFormatIsRefused(t *testing.T) {
	_, err := Parse(map[string]any{"schema": map[string]any{"type": "object"}})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Parse of a schema under the text default error = %v, want ErrInvalid", err)
	}
}

func TestJSONSchemaFormatRequiresASchema(t *testing.T) {
	_, err := Parse(map[string]any{"format": "json_schema"})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("Parse of json_schema with no schema error = %v, want ErrInvalid", err)
	}
}

func TestParseDefaultsStrictAndName(t *testing.T) {
	c := mustParse(t, map[string]any{
		"format": "json_schema",
		"schema": map[string]any{"type": "object", "properties": map[string]any{}},
	})
	if !c.Strict {
		t.Fatalf("strict defaulted to %v, want true", c.Strict)
	}
	if c.Name != "output" {
		t.Fatalf("name defaulted to %q, want %q", c.Name, "output")
	}
	if !c.Demanded() {
		t.Fatal("Demanded() = false for a json_schema contract with a schema")
	}
}

// --- the fail-closed rule -------------------------------------------------------------------

// TestCheckRefusesEveryKeywordValidateDoesNotEnforce is the package's central invariant. Anything
// Check accepts, Validate enforces exactly; so a keyword Validate ignores MUST be refused up front.
// Without this, adding a keyword to the published schema and forgetting the validator produces the
// worst possible outcome: a caller's constraint silently not applied, and a `completed` run.
func TestCheckRefusesEveryKeywordValidateDoesNotEnforce(t *testing.T) {
	unenforced := []string{
		"$ref", "$defs", "$id", "$schema", "$dynamicRef",
		"oneOf", "anyOf", "allOf", "not", "if", "then", "else",
		"pattern", "format", "patternProperties", "propertyNames",
		"uniqueItems", "prefixItems", "contains", "const",
		"exclusiveMinimum", "exclusiveMaximum", "multipleOf",
		"dependentRequired", "dependentSchemas", "unevaluatedProperties",
	}
	for _, keyword := range unenforced {
		schema := map[string]any{"type": "object", "properties": map[string]any{}, keyword: "anything"}
		if err := Check(schema); err == nil {
			t.Fatalf("Check accepted %q, but Validate does not enforce it — an accepted schema that is "+
				"not fully checked returns valid for output that is not", keyword)
		}
	}
}

// TestCheckRefusesAnUntypedSchema — an untyped schema constrains nothing, so accepting one would
// make every answer valid while the caller believes a contract is in force.
func TestCheckRefusesAnUntypedSchema(t *testing.T) {
	if err := Check(map[string]any{"properties": map[string]any{}}); err == nil {
		t.Fatal("Check accepted a schema with no type")
	}
	if err := Check(map[string]any{"type": []any{"string", "null"}}); err == nil {
		t.Fatal("Check accepted a union type, which Validate does not implement")
	}
}

func TestCheckRefusesUnsupportedTypeAndDeepNesting(t *testing.T) {
	if err := Check(map[string]any{"type": "date"}); err == nil {
		t.Fatal("Check accepted an unsupported type")
	}
	deep := map[string]any{"type": "string"}
	for i := 0; i < maxSchemaDepth+2; i++ {
		deep = map[string]any{"type": "object", "properties": map[string]any{"n": deep}, "required": []any{"n"}}
	}
	if err := Check(deep); err == nil {
		t.Fatalf("Check accepted a schema nested deeper than %d", maxSchemaDepth)
	}
}

func TestCheckRefusesRequiredPropertyThatIsNotDefined(t *testing.T) {
	schema := map[string]any{
		"type":       "object",
		"properties": map[string]any{"a": map[string]any{"type": "string"}},
		"required":   []any{"b"},
	}
	if err := Check(schema); err == nil {
		t.Fatal("Check accepted a schema requiring an undefined property — nothing could ever satisfy it")
	}
}

func TestCheckAcceptsTheSupportedSubset(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":       map[string]any{"type": "string", "minLength": float64(1), "maxLength": float64(80)},
			"population": map[string]any{"type": "integer", "minimum": float64(0)},
			"tags":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": float64(5)},
			"capital":    map[string]any{"type": "boolean"},
			"region":     map[string]any{"type": "string", "enum": []any{"eu", "asia"}},
		},
		"required":             []any{"city", "population"},
		"additionalProperties": false,
	}
	if err := Check(schema); err != nil {
		t.Fatalf("Check rejected the supported subset: %v", err)
	}
}

// --- validation -----------------------------------------------------------------------------

func supportedSchema() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"city":       map[string]any{"type": "string"},
			"population": map[string]any{"type": "integer"},
		},
		"required":             []any{"city", "population"},
		"additionalProperties": false,
	}
}

// TestValidateTextRejectsTheProseThatShippedForThreeYears is the unit-level twin of the e2e RED: the
// literal answer the live stack returned on 2026-08-01 for a request that demanded this schema.
func TestValidateTextRejectsTheProseThatShippedForThreeYears(t *testing.T) {
	prose := "Ankara is the capital city of Turkey. As of my last update in October 2023, " +
		"the approximate population of Ankara is around 5.7 million people."
	err := ValidateText(supportedSchema(), prose)
	if err == nil {
		t.Fatal("ValidateText accepted prose against an object schema")
	}
	if !strings.Contains(err.Error(), "not JSON") {
		t.Fatalf("error %q should say the answer was not JSON at all", err)
	}
}

func TestValidateTextAcceptsAConformingAnswer(t *testing.T) {
	if err := ValidateText(supportedSchema(), `{"city":"Ankara","population":5663000}`); err != nil {
		t.Fatalf("ValidateText rejected a conforming answer: %v", err)
	}
}

func TestValidateNamesTheExactFailure(t *testing.T) {
	cases := []struct{ name, doc, want string }{
		{"missing required", `{"city":"Ankara"}`, `missing required property "population"`},
		{"wrong type", `{"city":"Ankara","population":"lots"}`, "should be an integer"},
		{"fractional integer", `{"city":"Ankara","population":1.5}`, "should be an integer"},
		{"extra property", `{"city":"Ankara","population":1,"x":2}`, `does not permit`},
		{"not an object", `["Ankara"]`, "should be an object"},
		{"empty", `   `, "no output"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateText(supportedSchema(), tc.doc)
			if err == nil {
				t.Fatalf("ValidateText(%s) = nil, want an error", tc.doc)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateText(%s) error = %q, want it to contain %q", tc.doc, err, tc.want)
			}
		})
	}
}

func TestValidateChecksNestedAndArrayAndEnum(t *testing.T) {
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"meta": map[string]any{
				"type":       "object",
				"properties": map[string]any{"region": map[string]any{"type": "string", "enum": []any{"eu", "asia"}}},
				"required":   []any{"region"},
			},
			"tags": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "maxItems": float64(2)},
		},
		"required": []any{"meta"},
	}
	if err := Check(schema); err != nil {
		t.Fatalf("Check rejected the nested schema: %v", err)
	}
	if err := ValidateText(schema, `{"meta":{"region":"eu"},"tags":["a"]}`); err != nil {
		t.Fatalf("ValidateText rejected a conforming nested answer: %v", err)
	}
	if err := ValidateText(schema, `{"meta":{"region":"mars"}}`); err == nil {
		t.Fatal("enum violation accepted")
	}
	if err := ValidateText(schema, `{"meta":{"region":"eu"},"tags":["a","b","c"]}`); err == nil {
		t.Fatal("maxItems violation accepted")
	}
	if err := ValidateText(schema, `{"meta":{"region":"eu"},"tags":[1]}`); err == nil {
		t.Fatal("array item type violation accepted")
	}
}

// TestValidateErrorsAreInTheCallersVocabulary — a caller reading a 4xx should see their own document
// described, not our decoder's Go types.
func TestValidateErrorsAreInTheCallersVocabulary(t *testing.T) {
	err := ValidateText(supportedSchema(), `{"city":123,"population":1}`)
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), "float64") || strings.Contains(err.Error(), "interface {}") {
		t.Fatalf("error %q leaks Go types to a caller who sent JSON", err)
	}
	if !strings.Contains(err.Error(), "output.city") {
		t.Fatalf("error %q does not point at the offending field", err)
	}
}
