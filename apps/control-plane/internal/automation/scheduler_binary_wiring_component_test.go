//go:build component

// External-package (automation_test) component test: the schedule ticker wired into main.go's OWN router +
// supervisor configuration, over real PostgreSQL. It proves the E09/E10/E11-T2 binary-wiring lesson for
// E11 T3 — the headline feature (a scheduled firing) runs under the SAME NewRouter seam list AND a
// supervised loop named exactly "schedule-ticker" (a SIBLING of "delivery-reconciler"), not an isolated
// harness. It lives in package automation_test so it can import api + store.
package automation_test

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/automation"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
)

// TestScheduleTickerWiredIntoRunningBinary proves the schedule tick runs under main.go's own router +
// supervisor config: a schedule POSTed over the real router, once due, materializes an occurrence,
// hands it to the trigger-delivery pipeline, and births a run visible at /v1/responses — all via the
// SUPERVISED "schedule-ticker" loop with NO manual tick. The migration chain runs via the binary's own
// Migrate (the embed wiring is proven free).
func TestScheduleTickerWiredIntoRunningBinary(t *testing.T) {
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
	org, project := seedScopedTenant(t, repo.Spine().Pool(), token)

	// main.go's OWN wiring: the same NewRouter seam list + the same NewScheduleStore + supervised
	// "schedule-ticker" (a fast interval for the test, the T2 binary-wiring precedent).
	pool := repo.Spine().Pool()
	webhookStore := automation.NewWebhookStore(pool)
	triggerStore := automation.NewTriggerStore(pool).WithAdmitter(repo.Spine())
	scheduleStore := automation.NewScheduleStore(pool, triggerStore)
	router := api.NewRouter(repo, repo, repo, repo, repo, repo, webhookStore, triggerStore, scheduleStore, nil, api.SSEConfig{}, nil, nil)
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)

	supervisor := coordinator.NewSupervisor(log.Printf, time.Second)
	ticker := automation.NewScheduleTicker(scheduleStore, 50*time.Millisecond, 100, nil)
	loopCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	go supervisor.Supervise(loopCtx, "schedule-ticker", ticker.Run)

	c := &client{t: t, base: srv.URL, token: token}

	// A published AgentRevision + a type='cron' trigger pinned to it (the run target). Seeded via the
	// automation package's own management API (in scope), then pinned through the real trigger route.
	revID := seedPublishedRevision(t, pool, org, project)
	triggerID := c.createCronTrigger()
	c.reviseTrigger(triggerID, `{"agent_revision_id":"`+revID+`","input_mapping":{"fields":{"input":{"const":"scheduled work"}}}}`)

	// POST a per-minute cron schedule over the real router (the create validates cron + timezone).
	scheduleID := c.createSchedule(`{"name":"` + randID("nightly") + `","trigger_id":"` + triggerID + `","cron_expr":"* * * * *","timezone":"UTC"}`)

	// Force it due NOW (accelerate the clock, not the machinery) so the supervised ticker fires it within a
	// tick — the occurrence + delivery + run are the real path.
	mustExec(t, pool, `UPDATE schedules SET next_fire_at = now() - interval '1 second' WHERE id=$1`, scheduleID)

	// The SUPERVISED ticker (no manual tick) drives: schedule → occurrence(admitted) → delivery → run.
	deliveryID := ""
	deadline := time.Now().Add(10 * time.Second)
	for {
		occs := c.scheduleOccurrences(scheduleID)
		for _, o := range occs {
			if o["state"] == "admitted" {
				if d, _ := o["delivery_id"].(string); d != "" {
					deliveryID = d
				}
			}
		}
		if deliveryID != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("schedule never admitted an occurrence via the supervised schedule-ticker; occurrences=%v", occs)
		}
		time.Sleep(50 * time.Millisecond)
	}

	// The occurrence handed off to a delivery that bore a run visible at /v1/responses — the full
	// schedule → occurrence → delivery → run chain, wired through main.go's seams.
	view := c.get("/v1/trigger-deliveries/" + deliveryID)
	var del map[string]any
	_ = json.NewDecoder(view.Body).Decode(&del)
	view.Body.Close()
	responseID, _ := del["response_id"].(string)
	if responseID == "" {
		t.Fatalf("scheduled delivery %s carried no response_id: %v", deliveryID, del)
	}
	resp := c.get("/v1/responses/" + responseID)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/responses/%s status = %d, want 200 (the run the scheduled occurrence bore)", responseID, resp.StatusCode)
	}
	resp.Body.Close()
}

// createCronTrigger registers a type='cron' trigger over the real router.
func (c *client) createCronTrigger() string {
	body := c.postJSON("/v1/triggers", `{"name":"`+randID("cron-trg")+`","type":"cron"}`, "")
	id, _ := body["id"].(string)
	if id == "" {
		c.t.Fatalf("createCronTrigger returned no id: %v", body)
	}
	return id
}

// createSchedule POSTs a schedule over the real router and returns its id.
func (c *client) createSchedule(body string) string {
	got := c.postJSON("/v1/schedules", body, "")
	id, _ := got["id"].(string)
	if id == "" {
		c.t.Fatalf("createSchedule returned no id: %v", got)
	}
	return id
}

// scheduleOccurrences reads a schedule's occurrence log over the real router.
func (c *client) scheduleOccurrences(scheduleID string) []map[string]any {
	resp := c.get("/v1/schedules/" + scheduleID + "/occurrences")
	defer resp.Body.Close()
	var body struct {
		Occurrences []map[string]any `json:"occurrences"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return body.Occurrences
}

// seedPublishedRevision creates + publishes an AgentRevision in scope via the automation management API.
func seedPublishedRevision(t *testing.T, pool *pgxpool.Pool, org, project string) string {
	t.Helper()
	ctx := context.Background()
	agents := automation.New(pool)
	profileID, err := agents.CreateProfile(ctx, org, project, randID("profile"))
	if err != nil {
		t.Fatalf("CreateProfile error = %v", err)
	}
	rev, err := agents.CreateRevision(ctx, org, project, profileID, []byte(`{"model":"gpt-4o-mini","instructions":"do the scheduled work"}`))
	if err != nil {
		t.Fatalf("CreateRevision error = %v", err)
	}
	if _, _, err := agents.PublishRevision(ctx, org, project, rev.ID); err != nil {
		t.Fatalf("PublishRevision error = %v", err)
	}
	return rev.ID
}

// seedScopedTenant creates org → project → principal → api_key (the token's stored verifier is its hash),
// returning the org/project the schedule + revision are seeded under.
func seedScopedTenant(t *testing.T, pool *pgxpool.Pool, token string) (org, project string) {
	t.Helper()
	ctx := context.Background()
	org, project, principal := randID("org"), randID("prj"), randID("prin")
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
	return org, project
}

// envOrSkip returns the component PG url or skips.
func envOrSkip(t *testing.T) string {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	return url
}
