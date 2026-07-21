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
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// callbackTrigger seeds a trigger whose revision shapes the run output through outputMapping and delivers
// the shaped callback to endpointID, admits one delivery, and pushes its response to a terminal completed
// with output — returning the admitted delivery.
func callbackTrigger(t *testing.T, store *TriggerStore, org, project, principal, name, endpointID, outputMapping, output string) DeliveryResult {
	t.Helper()
	ctx := context.Background()
	triggerID, _ := seedTrigger(t, store, org, project, name, TriggerRevisionInput{
		InputMapping:       []byte(`{"fields":{"input":{"const":"do the work"}}}`),
		OutputMapping:      json.RawMessage(outputMapping),
		CallbackEndpointID: endpointID,
	})
	del, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{}`))
	if err != nil {
		t.Fatalf("CreateDelivery error = %v", err)
	}
	if del.State != "run_created" {
		t.Fatalf("delivery state = %q, want run_created", del.State)
	}
	mustExec(t, store.pool, `UPDATE responses SET state='completed', output=$2::jsonb WHERE id=$1`, del.ResponseID, output)
	return del
}

// TestCallbackEnqueuedOnRunTerminal pins B3: when a delivery's run reaches terminal, the callback sweep
// shapes the response output through the SAME mapping language and enqueues ONE signed webhook delivery to
// the callback endpoint (callback_state → pending). Sweeping twice enqueues no second row (ON CONFLICT).
func TestCallbackEnqueuedOnRunTerminal(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	webhooks := NewWebhookStore(pool)
	endpointID, err := webhooks.CreateEndpoint(ctx, org, project, defaultEndpoint("https://cb.example/hook", "cbref"))
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}

	del := callbackTrigger(t, store, org, project, principal, "enqueue", endpointID,
		`{"fields":{"result":{"select":"output"}},"required":["result"]}`, `[{"type":"output_text","text":"done"}]`)

	if err := store.sweepCallbacks(ctx, 100, nil); err != nil {
		t.Fatalf("sweepCallbacks error = %v", err)
	}
	if err := store.sweepCallbacks(ctx, 100, nil); err != nil { // second sweep must not enqueue a second row
		t.Fatalf("second sweepCallbacks error = %v", err)
	}

	// Exactly one webhook_deliveries row for the callback endpoint, keyed cb:<deliveryID>, pending.
	var count int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM webhook_deliveries WHERE endpoint_id=$1 AND event_id=$2`, endpointID, "cb:"+del.ID).Scan(&count); err != nil {
		t.Fatalf("count callback deliveries error = %v", err)
	}
	if count != 1 {
		t.Fatalf("callback deliveries = %d, want exactly 1 (ON CONFLICT dedupe)", count)
	}
	if got := callbackState(t, pool, del.ID); got != "pending" {
		t.Fatalf("callback_state = %q, want pending", got)
	}

	// The envelope carries the SHAPED output as its data (trigger.callback.v1, data.result = the output).
	var payload []byte
	var eventType string
	if err := pool.QueryRow(ctx,
		`SELECT payload, event_type FROM webhook_deliveries WHERE event_id=$1`, "cb:"+del.ID).Scan(&payload, &eventType); err != nil {
		t.Fatalf("read callback payload error = %v", err)
	}
	if eventType != "trigger.callback.v1" {
		t.Fatalf("callback event_type = %q, want trigger.callback.v1", eventType)
	}
	var env struct {
		Type string `json:"type"`
		Data struct {
			Result []map[string]any `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(payload, &env); err != nil {
		t.Fatalf("decode callback envelope error = %v", err)
	}
	if env.Type != "trigger.callback.v1" || len(env.Data.Result) != 1 || env.Data.Result[0]["text"] != "done" {
		t.Fatalf("callback envelope did not carry the shaped output: %s", payload)
	}
}

// TestCallbackDeliveredOncePerDeliveryAcrossRetries pins B4: the callback is delivered through T4's pump; a
// 5xx-then-2xx receiver drives one retry under the SAME webhook id (the receiver dedupes on Webhook-Id →
// ONE semantic callback), and callback_state mirrors to delivered.
func TestCallbackDeliveredOncePerDeliveryAcrossRetries(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	secret := []byte("whsec_callback_once")

	var semanticCallbacks int32
	var mu sync.Mutex
	seen := map[string]bool{}
	srv, certs, calls, _ := tlsReceiverWithID(t, secret, func(attempt int, webhookID string) int {
		mu.Lock()
		if !seen[webhookID] {
			seen[webhookID] = true
			atomic.AddInt32(&semanticCallbacks, 1)
		}
		mu.Unlock()
		if attempt == 1 {
			return http.StatusServiceUnavailable // 503 first
		}
		return http.StatusOK // 200 after
	})

	webhooks := NewWebhookStore(pool)
	endpointID, err := webhooks.CreateEndpoint(ctx, org, project, defaultEndpoint(srv.URL, "cbref"))
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	del := callbackTrigger(t, store, org, project, principal, "deliver-once", endpointID,
		`{"fields":{"result":{"select":"status"}}}`, `[]`)

	if err := store.sweepCallbacks(ctx, 100, nil); err != nil {
		t.Fatalf("sweepCallbacks error = %v", err)
	}

	pump := pumpFor(webhooks, certs, secret, "cbref")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("pump Tick error = %v", err)
		}
		if err := store.sweepCallbacks(ctx, 100, nil); err != nil { // mirror the pump's terminal state
			t.Fatalf("mirror sweepCallbacks error = %v", err)
		}
		if callbackState(t, pool, del.ID) == "delivered" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := callbackState(t, pool, del.ID); got != "delivered" {
		t.Fatalf("callback_state = %q, want delivered", got)
	}
	if got := atomic.LoadInt32(&semanticCallbacks); got != 1 {
		t.Fatalf("semantic callbacks = %d, want exactly 1 (Webhook-Id dedupe across the retry)", got)
	}
	if atomic.LoadInt32(calls) < 2 {
		t.Fatalf("receiver saw %d HTTP calls, want >= 2 (503 then 200)", atomic.LoadInt32(calls))
	}
}

// TestCallbackFailureLeavesRunTerminalIntact pins B5 (AUT-011 link-half): a callback that dead-letters
// (permanent 500) sets callback_state=dead but leaves the run's response completed + output intact and the
// delivery state at run_created — the run result is recoverable even when the callback fails. A callback
// whose output-mapping fails at map time also dead-letters without a run, without enqueuing anything.
func TestCallbackFailureLeavesRunTerminalIntact(t *testing.T) {
	store, pool := wiredTriggerStore(t)
	ctx := context.Background()
	org, project, _ := seedSession(t, pool)
	principal := seedPrincipal(t, pool, org, project)
	secret := []byte("whsec_callback_dead")

	srv, certs, _, _ := tlsReceiver(t, secret, func(int) int { return http.StatusInternalServerError })
	webhooks := NewWebhookStore(pool)
	ep := defaultEndpoint(srv.URL, "cbref")
	ep.MaxAttempts = 2 // dead-letter quickly
	endpointID, err := webhooks.CreateEndpoint(ctx, org, project, ep)
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	del := callbackTrigger(t, store, org, project, principal, "dead-letter", endpointID,
		`{"fields":{"result":{"select":"output"}}}`, `[{"type":"output_text","text":"canonical"}]`)

	if err := store.sweepCallbacks(ctx, 100, nil); err != nil {
		t.Fatalf("sweepCallbacks error = %v", err)
	}
	pump := pumpFor(webhooks, certs, secret, "cbref")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("pump Tick error = %v", err)
		}
		if err := store.sweepCallbacks(ctx, 100, nil); err != nil {
			t.Fatalf("mirror sweepCallbacks error = %v", err)
		}
		if callbackState(t, pool, del.ID) == "dead" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := callbackState(t, pool, del.ID); got != "dead" {
		t.Fatalf("callback_state = %q, want dead", got)
	}
	// The run result is INTACT and independent of the callback failure (AUT-011 link-half).
	var respState string
	var output []byte
	if err := pool.QueryRow(ctx, `SELECT state, output FROM responses WHERE id=$1`, del.ResponseID).Scan(&respState, &output); err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if respState != "completed" {
		t.Fatalf("response state = %q, want completed (callback failure must not corrupt the run)", respState)
	}
	if len(output) == 0 || !json.Valid(output) {
		t.Fatalf("response output was scrubbed by the callback failure: %s", output)
	}
	if got := deliveryState(t, pool, del.ID); got != "run_created" {
		t.Fatalf("delivery state = %q, want run_created (the callback has its OWN terminal)", got)
	}

	// Sub-case: an output-mapping that fails at callback time dead-letters WITHOUT enqueuing a delivery,
	// leaving the run intact.
	del2 := callbackTrigger(t, store, org, project, principal, "map-fail", endpointID,
		`{"fields":{"result":{"select":"missing"}},"required":["result"]}`, `[]`) // required field absent → Apply fails
	if err := store.sweepCallbacks(ctx, 100, nil); err != nil {
		t.Fatalf("map-fail sweepCallbacks error = %v", err)
	}
	if got := callbackState(t, pool, del2.ID); got != "dead" {
		t.Fatalf("map-fail callback_state = %q, want dead", got)
	}
	var enqueued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM webhook_deliveries WHERE event_id=$1`, "cb:"+del2.ID).Scan(&enqueued); err != nil {
		t.Fatalf("count map-fail deliveries error = %v", err)
	}
	if enqueued != 0 {
		t.Fatalf("a map-failed callback enqueued %d deliveries, want 0", enqueued)
	}
	var resp2State string
	if err := pool.QueryRow(ctx, `SELECT state FROM responses WHERE id=$1`, del2.ResponseID).Scan(&resp2State); err != nil {
		t.Fatalf("read map-fail response error = %v", err)
	}
	if resp2State != "completed" {
		t.Fatalf("map-fail response state = %q, want completed (mapping failure must not corrupt the run)", resp2State)
	}
}

// callbackState reads a delivery's callback_state column.
func callbackState(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT callback_state FROM trigger_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read callback_state error = %v", err)
	}
	return state
}
