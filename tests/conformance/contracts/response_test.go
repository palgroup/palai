package contracts_test

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

func TestResponseFixtureRoundTripsWithoutLosingOmittedFields(t *testing.T) {
	// A response with no error must serialize without an explicit "error" key:
	// the open envelope omits what is absent rather than emitting null.
	r := contracts.Response{
		ID:        contracts.ResponseID("resp_abc123"),
		Object:    "response",
		Status:    "completed",
		CreatedAt: "2026-07-16T12:00:00Z",
		Model:     "palai-sonnet",
		Output:    []contracts.ContentItem{{"type": "output_text", "text": "hi"}},
		Usage:     contracts.Usage{InputTokens: 3, OutputTokens: 5},
	}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if _, present := raw["error"]; present {
		t.Fatalf("omitted error serialized as a key: %s", data)
	}

	var round contracts.Response
	roundTrip(t, r, &round)
	if round.ID != r.ID || round.Object != r.Object || round.Status != r.Status ||
		round.CreatedAt != r.CreatedAt || round.Model != r.Model {
		t.Fatalf("round-trip lost required fields: %+v", round)
	}
	if len(round.Output) != 1 || round.Output[0].Type() != "output_text" {
		t.Fatalf("round-trip lost output: %+v", round.Output)
	}
	if round.Usage.InputTokens != 3 || round.Usage.OutputTokens != 5 {
		t.Fatalf("round-trip lost usage: %+v", round.Usage)
	}
}

func TestResponseCreateRejectsBothContinuationKeys(t *testing.T) {
	schema := readSchema(t, "execution/response-create.json")
	allOf, ok := schema["allOf"].([]any)
	if !ok || len(allOf) == 0 {
		t.Fatal("response-create.json declares no allOf constraints")
	}
	// The continuation keys are mutually exclusive: an allOf `not` forbids any
	// payload that carries previous_response_id and session_id together.
	forbidden := false
	for _, raw := range allOf {
		entry, _ := raw.(map[string]any)
		not, ok := entry["not"].(map[string]any)
		if !ok {
			continue
		}
		req := schemaStrings(t, not["required"])
		if slices.Contains(req, "previous_response_id") && slices.Contains(req, "session_id") {
			forbidden = true
		}
	}
	if !forbidden {
		t.Fatal("response-create.json does not forbid previous_response_id + session_id together")
	}

	// The generated request type carries input plus both optional continuation
	// keys, so a caller can still set either one on its own.
	prev := "resp_prev123"
	in := contracts.ResponseCreateRequest{Input: "hello", PreviousResponseID: &prev}
	var round contracts.ResponseCreateRequest
	roundTrip(t, in, &round)
	if round.Input != "hello" {
		t.Fatalf("round-trip lost input: %+v", round)
	}
	if round.PreviousResponseID == nil || *round.PreviousResponseID != prev {
		t.Fatalf("round-trip lost previous_response_id: %v", round.PreviousResponseID)
	}
}

func TestResponseStatusCoversSpecLifecycle(t *testing.T) {
	schema := readSchema(t, "execution/response.json")
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatal("response.json has no properties")
	}
	status, ok := props["status"].(map[string]any)
	if !ok {
		t.Fatal("response.json has no status property")
	}
	// Status is an open string: the lifecycle values are documented in $defs,
	// not fixed by an exclusive enum on the property (spec §20.6, API-009).
	if status["type"] != "string" {
		t.Fatalf("status.type = %v, want string", status["type"])
	}
	if _, closed := status["enum"]; closed {
		t.Fatal("status property carries an exclusive enum; the lifecycle must stay open")
	}
	defs, _ := schema["$defs"].(map[string]any)
	known, ok := defs["known_statuses"].(map[string]any)
	if !ok {
		t.Fatal("response.json $defs.known_statuses is missing")
	}
	// spec §8.3 response lifecycle, in progression order.
	want := []string{
		"queued", "provisioning", "in_progress", "waiting_for_tool",
		"waiting_for_approval", "waiting_for_input", "completed", "failed",
		"canceled", "timed_out", "budget_exceeded",
	}
	got := schemaStrings(t, known["enum"])
	if !slices.Equal(got, want) {
		t.Fatalf("known_statuses\n got = %v\nwant = %v", got, want)
	}
}

func TestUsageRequiresTokenCounts(t *testing.T) {
	schema := readSchema(t, "execution/usage.json")
	got := schemaStrings(t, schema["required"])
	want := []string{"input_tokens", "output_tokens"}
	if !slices.Equal(got, want) {
		t.Fatalf("usage.json required = %v, want %v", got, want)
	}

	// The generated type carries the counts as integers and round-trips them.
	u := contracts.Usage{InputTokens: 12, OutputTokens: 7, TotalTokens: 19}
	var round contracts.Usage
	roundTrip(t, u, &round)
	if round.InputTokens != 12 || round.OutputTokens != 7 || round.TotalTokens != 19 {
		t.Fatalf("round-trip lost token counts: %+v", round)
	}
}
