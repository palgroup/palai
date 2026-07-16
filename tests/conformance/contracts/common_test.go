package contracts_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
)

func readSchema(t *testing.T, rel string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(t), "protocols/schemas", rel))
	if err != nil {
		t.Fatal(err)
	}
	var schema map[string]any
	if err := json.Unmarshal(data, &schema); err != nil {
		t.Fatal(err)
	}
	return schema
}

func schemaStrings(t *testing.T, raw any) []string {
	t.Helper()
	list, ok := raw.([]any)
	if !ok {
		t.Fatalf("expected a JSON array, got %T", raw)
	}
	out := make([]string, len(list))
	for i, v := range list {
		s, ok := v.(string)
		if !ok {
			t.Fatalf("expected string element, got %T", v)
		}
		out[i] = s
	}
	return out
}

func roundTrip[T any](t *testing.T, in T, out *T) {
	t.Helper()
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		t.Fatal(err)
	}
}

func TestProblemRequiresStableCodeAndRequestID(t *testing.T) {
	// The schema marks the five load-bearing fields required, so a fixture that
	// omits any of them cannot validate.
	schema := readSchema(t, "common/problem.json")
	got := schemaStrings(t, schema["required"])
	want := []string{"type", "title", "status", "code", "request_id"}
	if !slices.Equal(got, want) {
		t.Fatalf("problem.json required = %v, want %v", got, want)
	}

	// The generated Go type carries those fields and types request_id as the
	// canonical id.json request_id.
	p := contracts.Problem{
		Type:      "https://docs.palai.dev/problems/not-found",
		Title:     "Not found",
		Status:    404,
		Code:      "not_found",
		RequestID: contracts.RequestID("req_abc123"),
	}
	if !p.RequestID.Valid() {
		t.Fatalf("request_id %q is not a valid RequestID", p.RequestID)
	}
	var round contracts.Problem
	roundTrip(t, p, &round)
	if round.Type != p.Type || round.Title != p.Title || round.Status != p.Status ||
		round.Code != p.Code || round.RequestID != p.RequestID {
		t.Fatalf("round-trip lost required fields: %+v", round)
	}
}

func TestProblemStableCodesAreDocumented(t *testing.T) {
	// spec §20.10 stable error family table, in HTTP-status reading order.
	documented := []string{
		"invalid_request", "invalid_state", "unsupported_content", "missing_idempotency_key",
		"authentication_required", "invalid_token", "expired_token",
		"permission_denied", "capability_denied", "policy_denied", "region_denied",
		"not_found",
		"revision_conflict", "idempotency_mismatch", "idempotency_in_progress", "active_run_conflict", "lease_conflict",
		"gone", "idempotency_result_expired", "retention_expired",
		"precondition_failed",
		"payload_too_large", "context_too_large",
		"schema_validation_failed", "unsupported_model_capability",
		"rate_limited", "quota_exceeded", "concurrency_exceeded",
		"internal_error",
		"provider_error", "tool_transport_error", "runner_error",
		"capacity_unavailable", "dependency_unavailable", "maintenance",
		"operation_timed_out",
	}
	schema := readSchema(t, "common/problem.json")
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("problem.json has no $defs")
	}
	knownCodes, ok := defs["known_codes"].(map[string]any)
	if !ok {
		t.Fatal("problem.json $defs.known_codes is missing")
	}
	got := schemaStrings(t, knownCodes["enum"])
	if !slices.Equal(got, documented) {
		t.Fatalf("known_codes\n got = %v\nwant = %v", got, documented)
	}
}

func TestResourceEnvelopeTimestampsAreRFC3339(t *testing.T) {
	// The schema types timestamps as date-time strings.
	schema := readSchema(t, "common/resource.json")
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("resource.json has no $defs")
	}
	ts, ok := defs["timestamp"].(map[string]any)
	if !ok {
		t.Fatal("resource.json $defs.timestamp is missing")
	}
	if ts["type"] != "string" || ts["format"] != "date-time" {
		t.Fatalf("timestamp def = %v, want {string, date-time}", ts)
	}

	// The generated Go type round-trips RFC 3339 UTC timestamps.
	r := contracts.ResourceEnvelope{
		ID:        contracts.OpaqueID("res_abc123"),
		Object:    "resource",
		CreatedAt: "2026-07-16T12:00:00.000000Z",
		UpdatedAt: "2026-07-16T12:30:00.000000Z",
	}
	var round contracts.ResourceEnvelope
	roundTrip(t, r, &round)
	for _, stamp := range []string{round.CreatedAt, round.UpdatedAt} {
		if _, err := time.Parse(time.RFC3339, stamp); err != nil {
			t.Fatalf("timestamp %q is not RFC 3339: %v", stamp, err)
		}
		if !strings.HasSuffix(stamp, "Z") {
			t.Fatalf("timestamp %q is not UTC (missing Z)", stamp)
		}
	}
}

func TestPageRequiresDataAndHasMore(t *testing.T) {
	schema := readSchema(t, "common/pagination.json")
	defs, ok := schema["$defs"].(map[string]any)
	if !ok {
		t.Fatal("pagination.json has no $defs")
	}
	page, ok := defs["page"].(map[string]any)
	if !ok {
		t.Fatal("pagination.json $defs.page is missing")
	}
	required := schemaStrings(t, page["required"])
	for _, field := range []string{"data", "has_more"} {
		if !slices.Contains(required, field) {
			t.Fatalf("page.required %v is missing %q", required, field)
		}
	}

	// The generated Go type carries data and has_more and round-trips a nullable cursor.
	next := "cursor_next"
	p := contracts.Page{
		Data:       []any{map[string]any{"id": "res_1"}},
		HasMore:    true,
		NextCursor: &next,
	}
	var round contracts.Page
	roundTrip(t, p, &round)
	if !round.HasMore || len(round.Data) != 1 {
		t.Fatalf("round-trip lost data/has_more: %+v", round)
	}
	if round.NextCursor == nil || *round.NextCursor != next {
		t.Fatalf("round-trip lost next_cursor: %v", round.NextCursor)
	}
}
