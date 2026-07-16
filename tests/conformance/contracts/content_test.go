package contracts_test

import (
	"encoding/json"
	"reflect"
	"slices"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// contentBranches maps each known content-item type to the `then` subschema its
// if/then branch enforces. Reading the branches from the schema keeps the test
// coupled to the canonical source rather than a hand-copied type list.
func contentBranches(t *testing.T, schema map[string]any) map[string]map[string]any {
	t.Helper()
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) == 0 {
		t.Fatal("content.json has no allOf branches (open union expected)")
	}
	if _, closed := schema["oneOf"]; closed {
		t.Fatal("content.json uses oneOf; a closed union rejects unknown types (API-009)")
	}
	branches := make(map[string]map[string]any, len(allOf))
	for _, raw := range allOf {
		entry, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("allOf entry is not an object: %T", raw)
		}
		ifObj, _ := entry["if"].(map[string]any)
		props, _ := ifObj["properties"].(map[string]any)
		typeProp, _ := props["type"].(map[string]any)
		constVal, ok := typeProp["const"].(string)
		if !ok {
			t.Fatalf("allOf branch does not gate on a type const: %v", entry)
		}
		then, _ := entry["then"].(map[string]any)
		branches[constVal] = then
	}
	return branches
}

// minimalFixture builds the smallest JSON object that satisfies a branch: the
// type discriminator plus a placeholder for every required field, typed to the
// field's declared JSON type so the fixture is structurally faithful.
func minimalFixture(typ string, then map[string]any) map[string]any {
	fixture := map[string]any{"type": typ}
	required, _ := then["required"].([]any)
	props, _ := then["properties"].(map[string]any)
	for _, r := range required {
		field, _ := r.(string)
		var kind string
		if def, ok := props[field].(map[string]any); ok {
			kind, _ = def["type"].(string)
		}
		switch kind {
		case "object":
			fixture[field] = map[string]any{"k": "v"}
		case "array":
			fixture[field] = []any{}
		case "integer", "number":
			fixture[field] = 1
		default:
			fixture[field] = "sample"
		}
	}
	return fixture
}

func TestKnownContentItemTypesValidate(t *testing.T) {
	schema := readSchema(t, "execution/content.json")
	branches := contentBranches(t, schema)

	want := []string{
		"artifact_ref", "audio_ref", "citation", "compacted_context",
		"file_ref", "image_ref", "input_text", "output_text", "redacted_content",
		"refusal", "structured_json", "tool_request", "tool_result", "warning",
	}
	got := make([]string, 0, len(branches))
	for typ := range branches {
		got = append(got, typ)
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Fatalf("known content-item types\n got = %v\nwant = %v", got, want)
	}

	// Every known type's minimal fixture round-trips through the generated Go
	// type without losing its discriminator or required fields.
	for typ, then := range branches {
		fixture := minimalFixture(typ, then)
		data, err := json.Marshal(fixture)
		if err != nil {
			t.Fatalf("%s: marshal fixture: %v", typ, err)
		}
		var item contracts.ContentItem
		if err := json.Unmarshal(data, &item); err != nil {
			t.Fatalf("%s: unmarshal into ContentItem: %v", typ, err)
		}
		if item.Type() != typ {
			t.Fatalf("%s: ContentItem.Type() = %q", typ, item.Type())
		}
		for field := range fixture {
			if _, ok := item[field]; !ok {
				t.Fatalf("%s: round-trip dropped required field %q", typ, field)
			}
		}
	}
}

func TestUnknownContentItemTypeRoundTripsPreserved(t *testing.T) {
	// An unknown type carrying an unknown field must survive a Go round-trip
	// intact: the open union never drops what it does not recognize (API-009).
	in := []byte(`{"type":"holo_ref","x":1}`)
	var item contracts.ContentItem
	if err := json.Unmarshal(in, &item); err != nil {
		t.Fatal(err)
	}
	if item.Type() != "holo_ref" {
		t.Fatalf("ContentItem.Type() = %q, want holo_ref", item.Type())
	}
	if _, ok := item["x"]; !ok {
		t.Fatal("unknown field x was dropped")
	}

	out, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var before, after map[string]any
	if err := json.Unmarshal(in, &before); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(out, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("round-trip changed the payload:\n before = %v\n after  = %v", before, after)
	}
}

func TestMessageRoleIsOpenEnum(t *testing.T) {
	schema := readSchema(t, "execution/message.json")
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("message.json has no properties")
	}
	role, ok := props["role"].(map[string]any)
	if !ok {
		t.Fatal("message.json has no role property")
	}
	// The role property is an open string: documented values live in $defs, not
	// an exclusive enum on the property itself.
	if role["type"] != "string" {
		t.Fatalf("role.type = %v, want string", role["type"])
	}
	if _, closed := role["enum"]; closed {
		t.Fatal("role property carries an exclusive enum; roles must stay open")
	}
	defs, _ := schema["$defs"].(map[string]any)
	knownRoles, ok := defs["known_roles"].(map[string]any)
	if !ok {
		t.Fatal("message.json $defs.known_roles is missing")
	}
	documented := schemaStrings(t, knownRoles["enum"])
	for _, want := range []string{"user", "assistant", "tool", "system_notice", "external_actor"} {
		if !slices.Contains(documented, want) {
			t.Fatalf("documented roles %v missing %q", documented, want)
		}
	}

	// The generated Go type keeps an unknown role rather than rejecting it.
	m := contracts.Message{
		ID:        contracts.MessageID("msg_abc123"),
		Role:      "holo_speaker",
		Content:   []contracts.ContentItem{{"type": "output_text", "text": "hi"}},
		CreatedAt: "2026-07-16T12:00:00Z",
	}
	var round contracts.Message
	roundTrip(t, m, &round)
	if round.Role != "holo_speaker" {
		t.Fatalf("round-trip changed role to %q", round.Role)
	}
	if len(round.Content) != 1 || round.Content[0].Type() != "output_text" {
		t.Fatalf("round-trip lost content: %+v", round.Content)
	}
}
