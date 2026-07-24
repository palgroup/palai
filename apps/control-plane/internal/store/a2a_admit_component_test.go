//go:build component

// Package store_test holds the real-PostgreSQL component proof that the production WithA2A wiring admits a
// genuine run through the REAL Admitter. It runs only under `make test-component TEST=postgres` (which starts
// a throwaway container and exports PALAI_COMPONENT_POSTGRES_URL); the build tag keeps it out of the
// credential-free, Docker-free unit tier.
package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/store"

	"github.com/palgroup/palai/storage"
)

// TestA2ARealAdmitterBirthsRun proves the production WithA2A wiring end to end against real PostgreSQL: an
// A2A message:send, routed through the REAL admission Admitter (store.Store — the SAME one POST /v1/responses
// uses, via api.NewA2AServer's a2aRuns adapter over the existing Admitter), BIRTHS a genuine canonical run
// under the AUTHENTICATED tenant scope and returns a task that resolves through the real GetResponse. No fake
// Runs — this closes the review's "a2a advertised but never served" gap with a real admission.
func TestA2ARealAdmitterBirthsRun(t *testing.T) {
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	ctx := context.Background()
	repo, err := store.Open(ctx, url) // the real api.Admitter
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	pool := repo.Spine().Pool()

	org, project := newID("org"), newID("prj")
	principalID := newID("prin")
	profileID, revID := newID("aprof"), newID("arev")
	exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, org)
	exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, project, org)
	exec(t, pool, `INSERT INTO principals (id, organization_id, project_id, kind) VALUES ($1,$2,$3,'service')`,
		principalID, org, project)
	exec(t, pool, `INSERT INTO agent_profiles (id, organization_id, project_id, name) VALUES ($1,$2,$3,$4)`,
		profileID, org, project, newID("name"))
	exec(t, pool, `INSERT INTO agent_revisions (id, organization_id, project_id, profile_id, revision_number, model, tools, published_at)
	               VALUES ($1,$2,$3,$4,1,'model-pinned','["file"]', clock_timestamp())`,
		revID, org, project, profileID)

	// Publish an A2A interface pinning the published revision (a SAFE card projection).
	a2aStore := a2a.NewStore(pool, newID)
	iface := a2a.ProjectInterface(revID,
		a2a.RevisionSource{Organization: org, Project: project, Model: "model-pinned"},
		a2a.PublishMeta{Name: "Real Planner", Version: "1", AuthScheme: "bearer",
			InputModes: []string{"text/plain"}, OutputModes: []string{"application/json"}})
	ifaceID, err := a2aStore.PublishInterface(ctx, iface)
	if err != nil {
		t.Fatalf("PublishInterface: %v", err)
	}

	srv := api.NewA2AServer(repo, a2aStore, a2aStore, api.AdmissionLimits{}, "https://cp.test")
	// This harness mounts no auth middleware, so inject the seeded tenant as the resolved scope (the
	// middleware.Auth -> ScopeFrom plumbing is separately proven by the api-package wiring test). The point
	// here is that the REAL admission births a run under exactly this scope — never a client-supplied one.
	srv.ScopeFunc = func(*http.Request) (a2a.Scope, bool) {
		return a2a.Scope{Organization: org, Project: project, Principal: principalID}, true
	}
	ts := httptest.NewServer(srv)
	defer ts.Close()

	base := ts.URL + "/v1/a2a/interfaces/" + ifaceID
	msg := map[string]any{"message": map[string]any{
		"role":      "user",
		"messageId": "real-1",
		"parts":     []map[string]any{{"kind": "text", "text": "plan my trip"}},
	}}
	blob, _ := json.Marshal(msg)
	resp, err := http.Post(base+"/message:send", "application/json", strings.NewReader(string(blob)))
	if err != nil {
		t.Fatalf("message:send: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("message:send = %d; %s", resp.StatusCode, body)
	}
	var task struct {
		ID     string `json:"id"`
		Kind   string `json:"kind"`
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := json.Unmarshal(body, &task); err != nil {
		t.Fatalf("unmarshal task: %v; %s", err, body)
	}
	if task.Kind != "task" || task.ID == "" {
		t.Fatalf("A2A message:send did not return a durable task: %s", body)
	}

	// A GENUINE run was born under the tenant scope: exactly one run row exists for this tenant.
	var runCount int
	if err := pool.QueryRow(storage.WithSystemScope(ctx),
		`SELECT count(*) FROM runs WHERE organization_id=$1 AND project_id=$2`, org, project).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("A2A message:send birthed %d runs under the tenant, want exactly 1", runCount)
	}

	// The task resolves through the REAL GetResponse (the ref bridges to the canonical response, §38.2).
	getResp, err := http.Get(base + "/tasks/" + task.ID)
	if err != nil {
		t.Fatalf("GET task: %v", err)
	}
	gb, _ := io.ReadAll(getResp.Body)
	getResp.Body.Close()
	if getResp.StatusCode != http.StatusOK {
		t.Fatalf("GET task = %d, want 200 (the born run must resolve); %s", getResp.StatusCode, gb)
	}
	var got struct {
		Status struct {
			State string `json:"state"`
		} `json:"status"`
	}
	_ = json.Unmarshal(gb, &got)
	if got.Status.State == "" || got.Status.State == string(a2a.TaskStateUnknown) {
		t.Fatalf("born run resolved to a non-live state %q; %s", got.Status.State, gb)
	}
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

func exec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(storage.WithSystemScope(context.Background()), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
