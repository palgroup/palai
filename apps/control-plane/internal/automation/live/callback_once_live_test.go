//go:build live

// CASE=callback-once (E11 Task 6, AUT-011 link-half + AUT-012): a callback-configured trigger delivery
// starts a REAL provider-one run pinned to a published AgentRevision; once the run reaches terminal, its
// output is shaped through the bounded output mapping and the signed callback is delivered to a REAL local
// receiver EXACTLY ONCE across an injected 5xx-then-2xx retry (the receiver verifies the HMAC server-side
// and dedupes on Webhook-Id). The run evidence stays intact and INDEPENDENT of the callback story.
//
// HONEST CEILING (mandatory, spec §20.2.2/§32.1, brief §6): the receiver is OUR harness — the proof class
// is the run-terminal/callback SEPARATION + the exactly-once delivery, not a third-party endpoint. The run
// run's effective tool set is empty (no default_tools configured), so dispatchModel advertises nothing and
// the run stays single-step + REAL; this case makes NO model→tool claim — that claim lives in the tool
// live cases. The post-irreversible guard is NOT exercised live (a live variant
// would need a forced tool call at the broker seam — tool_choice: required); it is proven in the component
// tier with seeded ledger rows (TestReplace/CoalesceDeniedAfterIrreversibleToolCall). The provider
// credential and the webhook signing secret are used as opaque needles for the leak scan and never printed.
package live

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestLiveCallbackOnce drives a callback-configured trigger through a REAL provider-one completion and
// proves the signed callback is delivered exactly once under a 5xx-then-2xx retry, with the run evidence
// intact and independent.
func TestLiveCallbackOnce(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-live-provider PROVIDER=provider-one CASE=callback-once")
	}
	ctx := context.Background()

	spine, err := coordinator.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(spine.Close)
	if err := spine.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := spine.Pool()
	org, project, principal := randID("org"), randID("prj"), randID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`, principal, org, project)

	// A published AgentRevision pinning the live model.
	agents := automation.New(pool)
	profileID, err := agents.CreateProfile(ctx, org, project, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"`+liveModel()+`","instructions":"summarize the order"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}

	// A REAL local TLS receiver: verifies the HMAC server-side, injects 5xx-then-2xx, and dedupes on the
	// Webhook-Id so a redelivery is ONE semantic callback.
	signingSecret := []byte("whsec_live_callback_once")
	var httpCalls int32
	var semantic int32
	var mu sync.Mutex
	seen := map[string]bool{}
	rcv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&httpCalls, 1)
		raw, _ := io.ReadAll(r.Body)
		id := r.Header.Get(webhook.HeaderID)
		ts, _ := strconv.ParseInt(r.Header.Get(webhook.HeaderTimestamp), 10, 64)
		if !webhook.Verify(signingSecret, id, time.Unix(ts, 0), raw, r.Header.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		if !seen[id] {
			seen[id] = true
			atomic.AddInt32(&semantic, 1)
		}
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusServiceUnavailable) // 503 first → one retry
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rcv.Close)
	certs := x509.NewCertPool()
	certs.AddCert(rcv.Certificate())

	// The callback endpoint + a trigger pinned to the revision, shaping the run output into the callback.
	webhookStore := automation.NewWebhookStore(pool)
	endpointID, err := webhookStore.CreateEndpoint(ctx, org, project, automation.EndpointCreate{
		URL: rcv.URL, EventFilter: []string{"trigger.callback.v1"}, SigningSecretRef: "cbref",
		TimeoutMS: 5000, MaxAttempts: 20, RetryWindowSeconds: 3600, AllowPrivateDestination: true,
	})
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}
	store := automation.NewTriggerStore(pool).WithAdmitter(spine)
	triggerID, err := store.CreateTrigger(ctx, org, project, principal, randID("orders"), "manual_api")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := store.ReviseTrigger(ctx, org, project, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID:    rev.ID,
		InputMapping:       []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
		OutputMapping:      []byte(`{"fields":{"result":{"select":"output"}},"required":["result"]}`),
		CallbackEndpointID: endpointID,
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}

	del, err := store.CreateDelivery(ctx, org, project, principal, triggerID, []byte(`{"order":{"id":"o-cb-1","summary":"fulfil the widget order"}}`))
	if err != nil {
		t.Fatalf("CreateDelivery error = %v", err)
	}
	if del.State != "run_created" || del.RunID == "" {
		t.Fatalf("delivery = %+v, want run_created with a run", del)
	}

	// Drive the REAL provider-one completion at the broker seam (the E08 pin: single-step, no tools).
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	mapped := mappedInputFor(t, pool, del.ID)
	res, err := broker.Route(ctx, "provider-one", modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(randID("mreq")),
		RouteRevision:  1, ModelStepID: "step-callback", Model: liveModel(),
		Messages:    []modelbroker.Message{{Role: "user", Content: "Summarize this triggered action in one short sentence: " + string(mapped)}},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route triggered run: %v", err)
	}
	assertRealCompletion(t, res)

	// The run reaches terminal: commit the real completion output onto the response (the canonical result).
	output, _ := json.Marshal([]map[string]any{{"type": "output_text", "text": res.Output}})
	if _, err := pool.Exec(ctx, `UPDATE responses SET state='completed', output=$2::jsonb WHERE id=$1`, del.ResponseID, output); err != nil {
		t.Fatalf("commit response output error = %v", err)
	}

	// The supervised loops in production; here we tick the sweep + pump directly until the callback settles.
	pump := automation.NewWebhookPump(webhookStore,
		webhook.NewSender(webhook.WithTLSConfig(&tls.Config{RootCAs: certs})),
		func(_ string, ref string) ([]byte, error) {
			if ref == "cbref" {
				return signingSecret, nil
			}
			return nil, io.EOF
		},
		automation.PumpConfig{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, BatchSize: 50}, nil)

	// The delivery-reconciler's Tick is what arms + mirrors the callback in production (its third step).
	rec := automation.NewDeliveryReconciler(store, time.Hour, time.Hour, 100, nil)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if err := rec.Tick(ctx); err != nil {
			t.Fatalf("reconciler Tick error = %v", err)
		}
		if err := pump.Tick(ctx); err != nil {
			t.Fatalf("pump Tick error = %v", err)
		}
		if callbackStateFor(t, pool, del.ID) == "delivered" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback never delivered (state=%q, http_calls=%d)", callbackStateFor(t, pool, del.ID), atomic.LoadInt32(&httpCalls))
		}
		time.Sleep(50 * time.Millisecond)
	}

	// (2) Exactly one semantic callback under the injected retry (the receiver saw >= 2 HTTP calls).
	if got := atomic.LoadInt32(&semantic); got != 1 {
		t.Fatalf("semantic callbacks = %d, want exactly 1 (Webhook-Id dedupe across the retry)", got)
	}
	if got := atomic.LoadInt32(&httpCalls); got < 2 {
		t.Fatalf("receiver saw %d HTTP calls, want >= 2 (the injected 503 retry then 200)", got)
	}

	// (3) The run evidence is INTACT and INDEPENDENT of the callback story.
	var respState string
	var respOutput []byte
	if err := pool.QueryRow(ctx, `SELECT state, output FROM responses WHERE id=$1`, del.ResponseID).Scan(&respState, &respOutput); err != nil {
		t.Fatalf("read response error = %v", err)
	}
	if respState != "completed" || len(respOutput) == 0 {
		t.Fatalf("run evidence not intact: state=%q output=%s", respState, respOutput)
	}
	if got := deliveryStateFor(t, pool, del.ID); got != "run_created" {
		t.Fatalf("delivery state = %q, want run_created (the callback has its OWN terminal)", got)
	}

	// (5) Leak scan by construction: neither the provider credential nor the signing secret is in evidence.
	blob := string(mustJSON(res)) + " " + string(respOutput)
	if strings.Contains(blob, secret) || strings.Contains(blob, string(signingSecret)) {
		t.Fatal("evidence surface contains a secret value")
	}

	t.Logf("live callback-once PASS (real provider-one, single-step, NO tool claim; run-terminal/callback separated): "+
		"run=%s chatcmpl=%s… model=%s http_calls=%d semantic_callbacks=1 callback_state=delivered",
		del.RunID, safePrefix(res.ProviderRequestID), res.Model, atomic.LoadInt32(&httpCalls))
}

// callbackStateFor reads a delivery's callback_state.
func callbackStateFor(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT callback_state FROM trigger_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read callback_state error = %v", err)
	}
	return state
}

// deliveryStateFor reads a delivery's pipeline state.
func deliveryStateFor(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var state string
	if err := pool.QueryRow(context.Background(), `SELECT state FROM trigger_deliveries WHERE id=$1`, id).Scan(&state); err != nil {
		t.Fatalf("read delivery state error = %v", err)
	}
	return state
}
