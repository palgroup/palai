package contracts_test

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

// sampleEvent builds a fully-populated envelope carrying every CloudEvents
// required attribute, so a round-trip test can assert none of them is lost.
func sampleEvent() contracts.Event {
	return contracts.Event{
		Specversion: "1.0",
		ID:          contracts.EventID("evt_abc123"),
		Source:      "palai",
		Type:        "session.created.v1",
		Time:        "2026-07-16T12:00:00Z",
		Sequence:    1,
		Data:        map[string]any{"k": "v"},
	}
}

func TestSessionEventSequenceMustBePositive(t *testing.T) {
	schema := readSchema(t, "execution/event.json")
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("event.json has no properties")
	}
	seq, ok := props["sequence"].(map[string]any)
	if !ok {
		t.Fatal("event.json has no sequence property")
	}
	// Per-session sequence is a monotonic counter starting at 1 (spec §13.2): the
	// schema pins the floor with an integer minimum of 1, so 0 or negative fails.
	if seq["type"] != "integer" {
		t.Fatalf("sequence.type = %v, want integer", seq["type"])
	}
	if seq["minimum"] != float64(1) {
		t.Fatalf("sequence.minimum = %v, want 1", seq["minimum"])
	}

	// The generated envelope carries the sequence as an int and round-trips it.
	e := sampleEvent()
	var round contracts.Event
	roundTrip(t, e, &round)
	if round.Sequence != 1 {
		t.Fatalf("round-trip lost sequence: %+v", round)
	}
}

func TestUnknownEventFieldIsIgnoredAndUnknownTypeIsPreserved(t *testing.T) {
	// API-009: the envelope is open. The schema tolerates additive fields...
	schema := readSchema(t, "execution/event.json")
	if schema["additionalProperties"] != true {
		t.Fatalf("event.json additionalProperties = %v, want true (open envelope)", schema["additionalProperties"])
	}

	// ...and the generated type ignores an unknown field on the way in while
	// preserving an unknown (forward-compatible) event type rather than coercing it.
	in := []byte(`{
		"specversion":"1.0",
		"id":"evt_abc123",
		"source":"palai",
		"type":"holo.materialized.v1",
		"time":"2026-07-16T12:00:00Z",
		"sequence":1,
		"data":{"k":"v"},
		"future_field":"surprise"
	}`)
	var e contracts.Event
	if err := json.Unmarshal(in, &e); err != nil {
		t.Fatalf("open envelope rejected an unknown field: %v", err)
	}
	if e.Type != "holo.materialized.v1" {
		t.Fatalf("unknown event type not preserved: %q", e.Type)
	}

	out, err := json.Marshal(e)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "future_field") {
		t.Fatalf("unknown field leaked back into the fixed envelope: %s", out)
	}
}

func TestEventTypeNamesAreVersioned(t *testing.T) {
	// event-types.json is a data registry (a name list), not a typed schema.
	registry := readSchema(t, "execution/event-types.json")
	names := schemaStrings(t, registry["events"])
	if len(names) == 0 {
		t.Fatal("event-types.json lists no events")
	}
	// Every registered name is a dotted, versioned identifier (spec §13.2):
	// lower_snake segments ending in an explicit .v<major>.
	versioned := regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*\.v[0-9]+$`)
	for _, name := range names {
		if !versioned.MatchString(name) {
			t.Errorf("event type %q is not versioned (want match %s)", name, versioned)
		}
	}
}

func TestEventEnvelopeMatchesCloudEventsRequiredSet(t *testing.T) {
	schema := readSchema(t, "execution/event.json")
	got := schemaStrings(t, schema["required"])
	// CloudEvents 1.0 core attributes the envelope always carries (spec §13.2).
	want := []string{"specversion", "id", "source", "type", "time", "sequence", "data"}
	if !slices.Equal(got, want) {
		t.Fatalf("event.json required\n got = %v\nwant = %v", got, want)
	}
	props, _ := schema["properties"].(map[string]any)
	specversion, _ := props["specversion"].(map[string]any)
	if specversion["const"] != "1.0" {
		t.Fatalf("specversion.const = %v, want \"1.0\"", specversion["const"])
	}

	// The generated envelope carries every required attribute and round-trips a
	// fully-populated event without losing them.
	e := sampleEvent()
	var round contracts.Event
	roundTrip(t, e, &round)
	if round.Specversion != e.Specversion || round.ID != e.ID || round.Source != e.Source ||
		round.Type != e.Type || round.Time != e.Time || round.Sequence != e.Sequence {
		t.Fatalf("round-trip lost required attributes: %+v", round)
	}
	if round.Data["k"] != "v" {
		t.Fatalf("round-trip lost data: %+v", round.Data)
	}
}
