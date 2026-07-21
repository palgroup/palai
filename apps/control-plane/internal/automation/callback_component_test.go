//go:build component

// Real-PostgreSQL component tests for trigger callbacks (spec §20.2.2, §21.6, §32.1, E11 Task 6). The
// callback is a post-run delivery: a triggered run reaches terminal, its output is shaped through the SAME
// bounded mapping language the input uses (no second language), and the shaped envelope is delivered to a
// registered webhook endpoint over T4's signed egress-safe pump — a normal webhook_deliveries row. The
// run's own terminal/evidence is INDEPENDENT of the callback (AUT-011 link-half): a callback that dead-
// letters never scrubs the run result. They run under `make test-component TEST=postgres`.
package automation

import (
	"context"
	"encoding/json"
	"testing"
)

// TestOutputMappingBoundedSameLanguageAsInput pins B1: a revise ACCEPTS an output_mapping compiled through
// the SAME bounded mapping language the input uses (an escape verb is rejected identically — no second
// language is invented), and a callback_endpoint_id is APP-SIDE scope-checked: the FK is global, so a
// foreign tenant's endpoint id must be a not-found reject or a run result would leak to a foreign URL.
func TestOutputMappingBoundedSameLanguageAsInput(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)

	webhooks := NewWebhookStore(pool)
	endpointID, err := webhooks.CreateEndpoint(ctx, org, project, defaultEndpoint("https://cb.example/hook", "cbref"))
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}

	triggerID, err := store.CreateTrigger(ctx, org, project, "with-callback", "manual_api")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}

	// A valid output_mapping (same language) + an in-scope callback endpoint is accepted and persisted.
	rev, err := store.ReviseTrigger(ctx, org, project, triggerID, TriggerRevisionInput{
		OutputMapping:      json.RawMessage(`{"fields":{"result":{"select":"output"}},"required":["result"]}`),
		CallbackEndpointID: endpointID,
	})
	if err != nil {
		t.Fatalf("ReviseTrigger(valid output+callback) error = %v", err)
	}
	var storedMapping []byte
	var storedEndpoint *string
	if err := pool.QueryRow(ctx,
		`SELECT output_mapping, callback_endpoint_id FROM trigger_revisions WHERE id=$1`, rev.ID).
		Scan(&storedMapping, &storedEndpoint); err != nil {
		t.Fatalf("read revision callback columns error = %v", err)
	}
	if storedEndpoint == nil || *storedEndpoint != endpointID {
		t.Fatalf("callback_endpoint_id = %v, want %q", storedEndpoint, endpointID)
	}
	if len(storedMapping) == 0 || string(storedMapping) == "{}" {
		t.Fatalf("output_mapping was not persisted: %s", storedMapping)
	}

	// The output_mapping is the SAME language as the input: an escape verb is rejected at compile, not run.
	if _, err := store.ReviseTrigger(ctx, org, project, triggerID, TriggerRevisionInput{
		OutputMapping: json.RawMessage(`{"fields":{"x":{"fetch":"http://169.254.169.254/"}}}`),
	}); err == nil {
		t.Fatal("an output_mapping carrying an escape verb was accepted; the mapping-language bound must reject it")
	}

	// SECURITY (the planner's catch): a callback_endpoint_id belonging to ANOTHER tenant is a not-found
	// reject — the global FK alone would let a run result be delivered to a foreign tenant's URL.
	otherOrg, otherProject, _ := seedSession(t, pool)
	foreignEndpoint, err := webhooks.CreateEndpoint(ctx, otherOrg, otherProject, defaultEndpoint("https://evil.example/steal", "evilref"))
	if err != nil {
		t.Fatalf("CreateEndpoint(foreign) error = %v", err)
	}
	if _, err := store.ReviseTrigger(ctx, org, project, triggerID, TriggerRevisionInput{
		CallbackEndpointID: foreignEndpoint,
	}); err == nil {
		t.Fatal("a foreign-tenant callback_endpoint_id was accepted; a run result would leak cross-tenant")
	}
}
