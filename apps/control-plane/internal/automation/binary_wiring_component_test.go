//go:build component

// External-package (automation_test) component test: the trigger delivery pipeline wired into main.go's
// OWN router + supervisor configuration, over real PostgreSQL. It lives in package automation_test (not
// automation) so it can import api + store — api imports automation, so an internal test cannot. This
// proves the E09/E10 binary-wiring lesson: the headline feature runs under the SAME NewRouter + supervised
// "delivery-reconciler" wiring main.go uses, not an isolated harness.
package automation_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
)

func randID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// TestTriggerDeliveryWiredIntoRunningBinary proves the delivery pipeline runs under main.go's own router +
// supervisor config: (a) a manual delivery POSTed over the real router births a run visible at
// /v1/responses/{id}; (b) a queue-deferred delivery reaches run_created via the SUPERVISED reconciler with
// NO manual tick. The migration chain runs via the binary's own Migrate (the embed wiring is proven free).
func TestTriggerDeliveryWiredIntoRunningBinary(t *testing.T) {
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
	token := randID("tok")
	seedTenantWithKey(t, repo.Spine().Pool(), token)

	// main.go's OWN wiring: the same NewRouter seam list + the same supervised "delivery-reconciler".
	webhookStore := automation.NewWebhookStore(repo.Spine().Pool())
	triggerStore := automation.NewTriggerStore(repo.Spine().Pool()).WithAdmitter(repo.Spine())
	router := api.NewRouter(repo, repo, repo, repo, repo, repo, webhookStore, triggerStore, nil, nil, nil, api.SSEConfig{}, nil, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)
	rec := automation.NewDeliveryReconciler(triggerStore, 50*time.Millisecond, time.Hour, 100, nil)
	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go supervisor.Supervise(loopCtx, "delivery-reconciler", rec.Run)

	c := &client{t: t, base: srv.URL, token: token}

	// (a) A manual delivery births a run visible via /v1/responses.
	triggerID := c.createTrigger("api-delivery")
	c.reviseTrigger(triggerID, `{"input_mapping":{"fields":{"input":{"const":"do the work"}}}}`)
	del := c.deliver(triggerID, `{}`, "idem-a")
	if del["state"] != "run_created" {
		t.Fatalf("delivery state = %v, want run_created", del["state"])
	}
	responseID, _ := del["response_id"].(string)
	if responseID == "" {
		t.Fatal("delivery carried no response_id")
	}
	resp := c.get("/v1/responses/" + responseID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/responses/%s status = %d, want 200 (the run the delivery bore)", responseID, resp.StatusCode)
	}
	resp.Body.Close()

	// (b) A queue-deferred delivery reaches run_created via the SUPERVISED reconciler (no manual tick).
	queued := c.createTrigger("queued")
	c.reviseTrigger(queued, `{"concurrency_policy":"queue","correlation_key_expr":{"select":"key"}}`)
	first := c.deliver(queued, `{"key":"k"}`, "idem-b1")
	if first["state"] != "run_created" {
		t.Fatalf("first queued delivery state = %v, want run_created", first["state"])
	}
	second := c.deliver(queued, `{"key":"k"}`, "idem-b2")
	if second["state"] != "deferred" {
		t.Fatalf("second queued delivery state = %v, want deferred", second["state"])
	}

	// Terminate the first run so the gate opens; the SUPERVISED loop must admit the deferred delivery.
	firstRun, _ := first["run_id"].(string)
	mustExec(t, repo.Spine().Pool(), `UPDATE runs SET state='completed' WHERE id=$1`, firstRun)

	secondID, _ := second["id"].(string)
	deadline := time.Now().Add(5 * time.Second)
	for {
		view := c.get("/v1/trigger-deliveries/" + secondID)
		var body map[string]any
		_ = json.NewDecoder(view.Body).Decode(&body)
		view.Body.Close()
		if body["state"] == "run_created" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("deferred delivery reached %v, want run_created via the supervised reconciler", body["state"])
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// client is a tiny authenticated HTTP client over the real router.
type client struct {
	t     *testing.T
	base  string
	token string
}

func (c *client) createTrigger(name string) string {
	body := c.postJSON("/v1/triggers", `{"name":"`+name+`"}`, "")
	id, _ := body["id"].(string)
	if id == "" {
		c.t.Fatalf("createTrigger returned no id: %v", body)
	}
	return id
}

func (c *client) reviseTrigger(triggerID, body string) {
	if got := c.postJSON("/v1/triggers/"+triggerID+"/revisions", body, ""); got["id"] == nil {
		c.t.Fatalf("reviseTrigger returned no revision: %v", got)
	}
}

func (c *client) deliver(triggerID, payload, idem string) map[string]any {
	return c.postJSON("/v1/triggers/"+triggerID+"/deliveries", payload, idem)
}

func (c *client) postJSON(path, body, idem string) map[string]any {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodPost, c.base+path, strings.NewReader(body))
	req.Close = true // fresh connection per request (avoid keep-alive body-reuse flakiness in the test)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	if idem != "" {
		req.Header.Set("Idempotency-Key", idem)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("POST %s error = %v", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(resp.Body)
		c.t.Fatalf("POST %s status = %d body = %s", path, resp.StatusCode, raw)
	}
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out
}

func (c *client) get(path string) *http.Response {
	c.t.Helper()
	req, _ := http.NewRequest(http.MethodGet, c.base+path, nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("GET %s error = %v", path, err)
	}
	return resp
}

// seedTenantWithKey creates org -> project -> principal -> api_key; the stored verifier is the hash of
// token, never token itself. It returns the org + project so a test can seed scope-owned rows directly.
func seedTenantWithKey(t *testing.T, pool *pgxpool.Pool, token string) (org, project string) {
	t.Helper()
	org, project, _ = seedTenantReturning(t, pool, token)
	return org, project
}

// seedTenantReturning is seedTenantWithKey that also returns the minted org/project/principal ids, for
// tests that must assert principal-scoped state (e.g. inbound created_by).
func seedTenantReturning(t *testing.T, pool *pgxpool.Pool, token string) (org, project, principal string) {
	t.Helper()
	ctx := context.Background()
	org, project, principal = randID("org"), randID("prj"), randID("prin")
	exec := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q error = %v", sql, err)
		}
	}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, project, org)
	exec(`INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1, $2, $3, 'service')`, principal, org, project)
	exec(`INSERT INTO api_keys (id, organization_id, project_id, principal_id, key_hash) VALUES ($1, $2, $3, $4, $5)`,
		randID("key"), org, project, principal, coordinator.HashAPIKey(token))
	return org, project, principal
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q error = %v", sql, err)
	}
}
