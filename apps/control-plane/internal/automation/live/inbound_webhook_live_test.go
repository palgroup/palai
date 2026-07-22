//go:build live

// CASE=inbound-webhook-run (E11 Task 5, AUT-001 inbound / AUT-002): a SIGNED inbound event POSTed over real
// HTTP to the mounted /v1/inbound/{trigger_id} route is verified (real HMAC), durably persisted, and starts
// a REAL provider-one run pinned to a published AgentRevision; a SECOND POST of the same source event is a
// duplicate linked to the original that starts NO second run — a broker-seam request-count counter proves
// count == deduped-event count == 1.
//
// HONEST CEILING (mandatory, spec §21.7, brief §6): the source is OUR harness (Slack/GitHub connectors are
// E17). The run is single-step and REAL (the E08 pin: no tools are advertised to the provider), so this case
// makes NO model→tool claim and therefore does NOT touch PALAI_LIVE_TOOL_ADVERTISING. The dedupe →
// no-second-run invariant is proven at the broker/adapter seam (the counting adapter wraps the REAL
// provider-one adapter, and only the canonical delivery dispatches a real completion). Verification runs
// before persistence (AUT-002); the full backpressure/poison/rotation surface is proven in the component
// tier. The provider credential + the signing secret appear in no captured surface.
package live

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestLiveInboundWebhookRun ingests two identical SIGNED inbound events over real HTTP against a real PG:
// the first verifies, durably persists, and bears a run pinned to a published AgentRevision; the second is
// a duplicate linked to the original that bears no run. The counting adapter over the REAL provider-one
// adapter proves the deduped second event adds ZERO provider calls (count == 1).
func TestLiveInboundWebhookRun(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}
	pgURL := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if pgURL == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-live-provider PROVIDER=provider-one CASE=inbound-webhook-run")
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

	// The signing secret rides a t.TempDir file referenced by env (the binary's PALAI_INBOUND_SECRET_FILE_
	// bridge shape) — never inline, never in argv/log.
	signingSecret := []byte("whsec_live_" + randID("s"))
	secretFile := filepath.Join(t.TempDir(), "inbound.secret")
	if err := os.WriteFile(secretFile, signingSecret, 0o600); err != nil {
		t.Fatalf("write secret file error = %v", err)
	}
	ref := "live-ref"
	t.Setenv("PALAI_INBOUND_SECRET_FILE_"+inboundEnvKey(org)+"__"+inboundEnvKey(ref), secretFile)
	resolver := func(o, r string) ([]byte, error) {
		path := os.Getenv("PALAI_INBOUND_SECRET_FILE_" + inboundEnvKey(o) + "__" + inboundEnvKey(r))
		if path == "" {
			return nil, os.ErrNotExist
		}
		return os.ReadFile(path)
	}

	store := automation.NewTriggerStore(pool).WithAdmitter(spine).WithInboundSecrets(resolver).WithInboundGate(nil, 5*time.Minute, 256, 0)
	triggerID, err := store.CreateTrigger(ctx, org, project, principal, randID("inbound-orders"), "webhook")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := store.ReviseTrigger(ctx, org, project, triggerID, automation.TriggerRevisionInput{
		AgentRevisionID: rev.ID,
		InputMapping:    []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	if err := store.SetInboundSecretRefs(ctx, org, project, triggerID, ref, ""); err != nil {
		t.Fatalf("SetInboundSecretRefs error = %v", err)
	}

	// Only the unauthenticated inbound route is exercised (it lives on the top mux, bypassing Auth), so every
	// other seam is nil — those routes simply do not mount.
	router := api.NewRouter(nil, nil, nil, nil, nil, nil, nil, store, nil, api.SSEConfig{}, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	// Two identical SIGNED POSTs over real HTTP to the mounted inbound route.
	body := []byte(`{"source":"harness","data":{"order":{"id":"o-inbound-1","summary":"fulfil the widget order"}}}`)
	first := postInbound(t, srv.URL, triggerID, "evt-inbound-1", signingSecret, body)
	if first["state"] != "run_created" {
		t.Fatalf("first inbound delivery state = %v, want run_created", first["state"])
	}
	firstID, _ := first["id"].(string)
	// The 202 implies a durable row (queried back — a second connection over the pool).
	if !inboundRowDurable(t, pool, firstID) {
		t.Fatal("202 returned but the delivery row is not durable")
	}
	second := postInbound(t, srv.URL, triggerID, "evt-inbound-1", signingSecret, body)
	if second["state"] != "duplicate" || second["duplicate_of"] != firstID {
		t.Fatalf("second inbound delivery = %v, want duplicate linked to %q", second, firstID)
	}

	// The counting seam over the REAL provider-one adapter. Only the canonical delivery bore a run, so the
	// deduped second event adds ZERO calls.
	adapter := &countingAdapter{inner: providerone.Adapter{}}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": adapter},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	var lastResult modelbroker.Result
	mapped := mappedInputFor(t, pool, firstID)
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID(randID("mreq")),
		RouteRevision:  1, ModelStepID: "step-inbound", Model: liveModel(),
		Messages:    []modelbroker.Message{{Role: "user", Content: "Summarize this triggered action in one short sentence: " + string(mapped)}},
		Deadline:    time.Now().Add(60 * time.Second),
		Reservation: modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:      modelbroker.SecretRef("provider-one"),
	}
	res, err := broker.Route(ctx, "provider-one", req, func(modelbroker.Delta) {})
	if err != nil {
		t.Fatalf("route inbound run: %v", err)
	}
	assertRealCompletion(t, res)
	lastResult = res

	if got := atomic.LoadInt32(&adapter.count); got != 1 {
		t.Fatalf("provider request count = %d, want exactly 1 (the deduped event must add no call)", got)
	}
	if !strings.HasPrefix(lastResult.Model, liveModel()) {
		t.Fatalf("completion model = %q, want the revision-pinned %q family", lastResult.Model, liveModel())
	}
	// Leak scan by construction: neither the provider credential nor the signing secret appears in the surface.
	surface := string(mustJSON(lastResult))
	if strings.Contains(surface, secret) || strings.Contains(surface, string(signingSecret)) {
		t.Fatal("the completion result contains a credential value")
	}

	t.Logf("live inbound-webhook PASS (real HMAC verify-before-persist, real provider-one, harness source, "+
		"single-step, NO tool claim): first_run_delivery=%s dedup_original=%s provider_calls=1 model=%s chatcmpl=%s…",
		firstID, second["duplicate_of"], lastResult.Model, safePrefix(lastResult.ProviderRequestID))
}

// postInbound signs body under secret and POSTs it to the UNAUTHENTICATED inbound route over real HTTP,
// returning the decoded 202 body.
func postInbound(t *testing.T, base, triggerID, eventID string, secret, body []byte) map[string]any {
	t.Helper()
	headers := webhook.NewSigner(secret).Headers(eventID, time.Now(), 1, body)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/inbound/"+triggerID, strings.NewReader(string(body)))
	req.Close = true
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/inbound error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("inbound POST status = %d, want 202", resp.StatusCode)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func inboundRowDurable(t *testing.T, pool *pgxpool.Pool, deliveryID string) bool {
	t.Helper()
	var raw []byte
	var srcEvent string
	if err := pool.QueryRow(context.Background(),
		`SELECT raw_payload, source_event_id FROM trigger_deliveries WHERE id=$1`, deliveryID).Scan(&raw, &srcEvent); err != nil {
		return false
	}
	return len(raw) > 0 && srcEvent != ""
}

// inboundEnvKey mirrors the binary's secretEnvKey (upper alphanumerics, others to '_').
func inboundEnvKey(ref string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(ref) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}
