//go:build component

// External-package (automation_test) component test: the trigger CALLBACK wired into main.go's OWN router
// + supervised loops, over real PostgreSQL. It proves the E09/E10 binary-wiring lesson for T6 — the
// callback runs under the SAME supervised "delivery-reconciler" (which arms + mirrors the callback) and
// "webhook-pump" (which signs + delivers it) that main.go wires, with NO manual Tick.
package automation_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
)

// TestCallbackWiredIntoRunningBinary proves the callback path runs end-to-end under main.go's own wiring: a
// callback-configured trigger delivery whose run reaches terminal has its signed callback delivered to a
// real local receiver by the SUPERVISED reconciler + pump chain — with NO manual Tick — and callback_state
// mirrors to delivered.
func TestCallbackWiredIntoRunningBinary(t *testing.T) {
	url := envOrSkip(t)
	ctx := context.Background()
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	token := randID("tok")
	org, project := seedTenantWithKey(t, repo.Spine().Pool(), token)

	// A real local TLS receiver that verifies the HMAC server-side (a sham verifier would not prove the
	// contract) and records the callback it accepts.
	secret := []byte("whsec_callback_binary_wiring")
	var received int32
	var gotType atomic.Value
	rcv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		id := r.Header.Get(webhook.HeaderID)
		ts, _ := strconv.ParseInt(r.Header.Get(webhook.HeaderTimestamp), 10, 64)
		if !webhook.Verify(secret, id, time.Unix(ts, 0), raw, r.Header.Get(webhook.HeaderSignature), time.Now(), 5*time.Minute) {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var env struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(raw, &env)
		gotType.Store(env.Type)
		atomic.AddInt32(&received, 1)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(rcv.Close)
	certs := x509.NewCertPool()
	certs.AddCert(rcv.Certificate())

	// main.go's OWN wiring: the same seam list + the SUPERVISED delivery-reconciler AND webhook-pump.
	webhookStore := automation.NewWebhookStore(repo.Spine().Pool())
	triggerStore := automation.NewTriggerStore(repo.Spine().Pool()).WithAdmitter(repo.Spine())
	router := api.NewRouter(repo, repo, repo, repo, repo, repo, webhookStore, triggerStore, nil, nil, nil, nil, nil, nil, api.SSEConfig{}, nil, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// A callback endpoint filtered to trigger.callback.v1 (the operator convention that keeps the general
	// journal fan-out off this endpoint; the callback delivery itself bypasses fan-out).
	endpointID, err := webhookStore.CreateEndpoint(ctx, org, project, automation.EndpointCreate{
		URL: rcv.URL, EventFilter: []string{"trigger.callback.v1"}, SigningSecretRef: "cbref",
		TimeoutMS: 3000, MaxAttempts: 20, RetryWindowSeconds: 3600, AllowPrivateDestination: true,
	})
	if err != nil {
		t.Fatalf("CreateEndpoint error = %v", err)
	}

	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)
	rec := automation.NewDeliveryReconciler(triggerStore, 40*time.Millisecond, time.Hour, 100, nil)
	pump := automation.NewWebhookPump(webhookStore, webhook.NewSender(webhook.WithTLSConfig(&tls.Config{RootCAs: certs})),
		func(_ string, ref string) ([]byte, error) {
			if ref == "cbref" {
				return secret, nil
			}
			return nil, io.EOF
		},
		automation.PumpConfig{BaseBackoff: time.Millisecond, MaxBackoff: time.Millisecond, Tick: 40 * time.Millisecond, BatchSize: 50}, nil)
	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go supervisor.Supervise(loopCtx, "delivery-reconciler", rec.Run)
	go supervisor.Supervise(loopCtx, "webhook-pump", pump.Run)

	c := &client{t: t, base: srv.URL, token: token}
	triggerID := c.createTrigger("callback-wired")
	c.reviseTrigger(triggerID, `{"input_mapping":{"fields":{"input":{"const":"do the work"}}},`+
		`"output_mapping":{"fields":{"result":{"select":"status"}}},"callback_endpoint_id":"`+endpointID+`"}`)
	del := c.deliver(triggerID, `{}`, "idem-cb")
	responseID, _ := del["response_id"].(string)
	deliveryID, _ := del["id"].(string)
	if responseID == "" || deliveryID == "" {
		t.Fatalf("delivery carried no ids: %v", del)
	}

	// The run reaches terminal (its response completes) — the callback fires from HERE, driven only by the
	// supervised loops.
	mustExec(t, repo.Spine().Pool(), `UPDATE responses SET state='completed', output='[]'::jsonb WHERE id=$1`, responseID)

	deadline := time.Now().Add(8 * time.Second)
	for {
		if atomic.LoadInt32(&received) >= 1 {
			view := c.get("/v1/trigger-deliveries/" + deliveryID)
			var body map[string]any
			_ = json.NewDecoder(view.Body).Decode(&body)
			view.Body.Close()
			if body["callback_state"] == "delivered" {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback not delivered via the supervised loops: received=%d", atomic.LoadInt32(&received))
		}
		time.Sleep(25 * time.Millisecond)
	}
	if got, _ := gotType.Load().(string); got != "trigger.callback.v1" {
		t.Fatalf("receiver got envelope type %q, want trigger.callback.v1", got)
	}
}
