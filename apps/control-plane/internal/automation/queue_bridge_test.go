package automation

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/queue"
	"github.com/palgroup/palai/adapters/integrations/webhook"
)

// TestQueueHTTPSinkDeliversOverTheVettedSender pins the outbound sink against a REAL local HTTP receiver:
// the body arrives verbatim, the destination key rides a header so the receiver can apply its own §34.5
// destination idempotency, and a 5xx is an error that leaves the delivery pending for the pump's retry.
func TestQueueHTTPSinkDeliversOverTheVettedSender(t *testing.T) {
	var gotKey, gotType string
	var gotBody []byte
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("Palai-Destination-Key")
		gotType = r.Header.Get("Content-Type")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)

	// allow_private is required for a loopback receiver — which is the point: the sink runs the egress
	// policy itself, it does not trust the row to have been vetted at create time.
	sinks := QueueHTTPSinks(webhook.NewSender())
	sink, err := sinks([]byte(`{"destination_url":"` + srv.URL + `/ingest","allow_private":true}`))
	if err != nil {
		t.Fatalf("resolve sink error = %v", err)
	}
	payload := []byte(`{"type":"run.terminal","run_id":"run_1"}`)
	if err := sink.Deliver(context.Background(), "run_1", payload); err != nil {
		t.Fatalf("Deliver error = %v, want nil", err)
	}
	if gotKey != "run_1" {
		t.Fatalf("destination key header = %q, want run_1", gotKey)
	}
	if gotType != "application/json" {
		t.Fatalf("content type = %q, want application/json", gotType)
	}
	if string(gotBody) != string(payload) {
		t.Fatalf("body = %q, want %q", gotBody, payload)
	}

	status = http.StatusInternalServerError
	if err := sink.Deliver(context.Background(), "run_2", payload); err == nil {
		t.Fatal("Deliver returned nil on a 5xx — the delivery would be marked delivered and lost")
	}
}

// TestQueueHTTPSinkRefusesUnvettedDestinations is the defence-in-depth half of the create-time egress gate:
// even if a row reached the table without passing the API (a direct DB write, a pre-vet row), the sink
// itself refuses a private destination that did not opt in, and refuses a connection with no destination
// at all rather than silently delivering nowhere.
func TestQueueHTTPSinkRefusesUnvettedDestinations(t *testing.T) {
	sinks := QueueHTTPSinks(webhook.NewSender())
	for _, tc := range []struct{ name, config string }{
		{"no destination", `{}`},
		{"empty config", ``},
		{"config is not an object", `"nope"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := sinks([]byte(tc.config)); err == nil {
				t.Fatal("resolver returned a sink for a connection with no usable destination")
			}
		})
	}

	// A loopback destination WITHOUT the self-host flag: the sender's own egress gate refuses it at
	// delivery time, so a row that bypassed the create-time vet still cannot reach a private address.
	sink, err := sinks([]byte(`{"destination_url":"http://127.0.0.1:9/ingest"}`))
	if err != nil {
		t.Fatalf("resolve sink error = %v", err)
	}
	if err := sink.Deliver(context.Background(), "run_1", []byte(`{}`)); err == nil {
		t.Fatal("Deliver reached a private destination that never opted in")
	}
}

// TestQueueRunInputIsAPureFunctionOfTheMessage pins the two properties the admission's idempotency depends
// on and the one the tenant isolation depends on:
//
//   - the input is byte-identical across DELIVERY ATTEMPTS of the same message, so a redelivery hashes the
//     same and REPLAYS instead of reporting an idempotency conflict (the Slack path's identical trap);
//   - the payload's source_tenant survives only as DATA, never as an identity — the connection id is what
//     the run carries;
//   - an unmappable body is an error the caller turns into a dead-letter, never a run.
func TestQueueRunInputIsAPureFunctionOfTheMessage(t *testing.T) {
	c := queueConn{id: "qconn_1", project: "prj_own"}
	body := []byte(`{"source":"orders.v1","source_tenant":"org_victim","source_event_id":"evt-1","data":{"n":1}}`)

	first, err := queueRunInput(c, queue.Message{Handle: "h1", IdempotencyKey: "k1", Body: body, Attempt: 1})
	if err != nil {
		t.Fatalf("queueRunInput error = %v", err)
	}
	// The SAME message on a later attempt with a different lease handle.
	second, err := queueRunInput(c, queue.Message{Handle: "h2", IdempotencyKey: "k1", Body: body, Attempt: 7})
	if err != nil {
		t.Fatalf("queueRunInput (redelivery) error = %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("the run input differs across delivery attempts:\n  %s\n  %s", first, second)
	}

	var in map[string]any
	if err := json.Unmarshal(first, &in); err != nil {
		t.Fatalf("run input is not JSON: %v", err)
	}
	if in["connection_id"] != "qconn_1" || in["idempotency_key"] != "k1" {
		t.Fatalf("run input = %v, want the connection id + the message key", in)
	}
	if in["source_tenant"] != "org_victim" {
		t.Fatalf("source_tenant = %v — it must survive as DATA so an operator can see what the producer claimed", in["source_tenant"])
	}
	if _, present := in["organization_id"]; present {
		t.Fatal("the run input carries an organization_id — a tenant must never be expressible in the payload projection")
	}

	for _, poison := range []string{`not json`, `{"data":{}}`, `[]`, ``} {
		if _, err := queueRunInput(c, queue.Message{IdempotencyKey: "k", Body: []byte(poison)}); err == nil {
			t.Fatalf("queueRunInput(%q) returned no error — an unmappable body would admit a run", poison)
		}
	}
}
