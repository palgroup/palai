//go:build component

package automation_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/webhook"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// envFileInboundResolver mirrors main.go's inboundSecretResolver (the PALAI_INBOUND_SECRET_FILE_<ORG>__<REF>
// bridge — a FILE PATH, never inline bytes), so the wiring test proves the SAME resolver shape the binary
// wires, not a bespoke closure.
func envFileInboundResolver(org, ref string) ([]byte, error) {
	path := os.Getenv("PALAI_INBOUND_SECRET_FILE_" + secretEnvKey(org) + "__" + secretEnvKey(ref))
	if path == "" {
		return nil, os.ErrNotExist
	}
	return os.ReadFile(path)
}

// secretEnvKey is main.go's env-suffix normalization (upper alphanumerics, others to '_').
func secretEnvKey(ref string) string {
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

// TestInboundWebhookWiredIntoRunningBinary proves E11 T5's headline feature runs under main.go's OWN router
// + supervisor config: (a) a harness-signed POST to /v1/inbound/{id} on the UNAUTHENTICATED top mux (no
// bearer) → 202 → the delivery reaches run_created (read with the key); (b) a durably-inserted pre-map
// inbound row reaches run_created via the SUPERVISED "delivery-reconciler" with NO manual Tick. The secret
// flows through the env-file bridge the binary uses.
func TestInboundWebhookWiredIntoRunningBinary(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()
	token := randID("tok")
	org, proj, principal := seedTenantReturning(t, pool, token)

	// The secret rides the env-file bridge, exactly as the binary resolves it.
	secret := []byte("whsec_wired_" + randID("s"))
	ref := "wired-ref"
	secretFile := filepath.Join(t.TempDir(), "inbound.secret")
	if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
		t.Fatalf("write secret file error = %v", err)
	}
	t.Setenv("PALAI_INBOUND_SECRET_FILE_"+secretEnvKey(org)+"__"+secretEnvKey(ref), secretFile)

	// main.go's OWN wiring: the resolver + gate on the store, the same NewRouter seam list, and the same
	// supervised "delivery-reconciler".
	triggerStore := automation.NewTriggerStore(pool).WithAdmitter(repo.Spine()).
		WithInboundSecrets(envFileInboundResolver).
		WithInboundGate(log.Printf, 0, 256, 0)
	router := api.NewRouter(repo, repo, repo, repo, repo, repo, automation.NewWebhookStore(pool), triggerStore, nil, nil, nil, nil, nil, api.SSEConfig{}, nil, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)
	rec := automation.NewDeliveryReconciler(triggerStore, 50*time.Millisecond, 0, 100, nil)
	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go supervisor.Supervise(loopCtx, "delivery-reconciler", rec.Run)

	// A webhook trigger created AS the principal, pinned to a revision, with the secret ref set.
	triggerID, err := triggerStore.CreateTrigger(ctx, org, proj, principal, randID("wired-inbound"), "webhook")
	if err != nil {
		t.Fatalf("CreateTrigger error = %v", err)
	}
	if _, err := triggerStore.ReviseTrigger(ctx, org, proj, triggerID, automation.TriggerRevisionInput{
		InputMapping: []byte(`{"fields":{"input":{"select":"order.summary"}},"required":["input"]}`),
	}); err != nil {
		t.Fatalf("ReviseTrigger error = %v", err)
	}
	if err := triggerStore.SetInboundSecretRefs(ctx, org, proj, triggerID, ref, ""); err != nil {
		t.Fatalf("SetInboundSecretRefs error = %v", err)
	}

	// (a) A harness-signed POST on the top mux (NO bearer) → 202, then run_created (read WITH the key).
	body := []byte(`{"source":"harness","data":{"order":{"id":"o-wired","summary":"do the wired work"}}}`)
	headers := webhook.NewSigner(secret).Headers("evt-wired", time.Now(), 1, body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/inbound/"+triggerID, strings.NewReader(string(body)))
	req.Close = true
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/inbound error = %v", err)
	}
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("inbound POST on the top mux (no bearer) status = %d, want 202", resp.StatusCode)
	}
	var acc map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&acc)
	resp.Body.Close()
	deliveryID, _ := acc["id"].(string)
	if deliveryID == "" {
		t.Fatalf("202 body carried no delivery id: %v", acc)
	}
	if view := getWiredDelivery(t, srv.URL, token, deliveryID); view["state"] != "run_created" {
		t.Fatalf("signed inbound delivery state = %v, want run_created", view["state"])
	}

	// (b) A durably-inserted pre-map inbound row reaches run_created via the SUPERVISED reconciler (no tick).
	var rev string
	if err := pool.QueryRow(storage.WithSystemScope(ctx), `SELECT id FROM trigger_revisions WHERE trigger_id=$1 ORDER BY revision_number DESC LIMIT 1`, triggerID).Scan(&rev); err != nil {
		t.Fatalf("read revision error = %v", err)
	}
	swept := randID("tdel")
	if _, err := pool.Exec(storage.WithSystemScope(ctx), `INSERT INTO trigger_deliveries
	        (id, organization_id, project_id, trigger_id, trigger_revision_id, principal_id,
	         source, source_tenant, source_event_id, raw_payload, state, received_at, updated_at)
	      VALUES ($1,$2,$3,$4,$5,$6,'harness','','evt-swept',$7,'received',
	              clock_timestamp() - interval '5 seconds', clock_timestamp() - interval '5 seconds')`,
		swept, org, proj, triggerID, rev, principal,
		[]byte(`{"source":"harness","data":{"order":{"id":"o-swept","summary":"swept work"}}}`)); err != nil {
		t.Fatalf("seed swept row error = %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		if view := getWiredDelivery(t, srv.URL, token, swept); view["state"] == "run_created" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("durably-inserted inbound row never reached run_created via the supervised reconciler")
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func getWiredDelivery(t *testing.T, base, token, id string) map[string]any {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, base+"/v1/trigger-deliveries/"+id, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET delivery error = %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}
