//go:build component

package store_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/storage"
)

// The E19 T9 Slack workspace REGISTRATION surface, end to end over real HTTP against REAL PostgreSQL and
// the shipped router — the operator path that did not exist before this task.
//
// WHY IT MATTERS BEYOND CONVENIENCE: E19 T1 closed a cross-tenant hijack at the store
// (CreateSlackConnection refuses a workspace already bound by a DIFFERENT tenant, because 000035's
// uniqueness is PER TENANT while the unauthenticated inbound resolve is keyed by team_id alone). That
// refusal was proven at the store. A registration surface is a NEW way to reach the same insert, so the
// refusal has to be re-proven THROUGH it — a handler that mapped it to a 500, or that echoed the store's
// error text, would either turn a security decision into "try again later" or tell the registering admin
// that another customer holds the workspace.
//
// HONEST CEILING, unchanged: this is evidence about our code and our database. Nothing here talks to
// slack.com, and `slack` stays PREVIEW — the external receipt is §6 leg 1.

// scopedVerifier resolves any bearer to one seeded tenant. The credential DB path is proven separately
// (identity component suite); what this file needs is a verified scope the router will trust, so the
// registration can be asserted to land in THAT tenant and not in one the body names.
type scopedVerifier struct{ scope middleware.Scope }

func (v scopedVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return v.scope, nil
}

// registrationFixture is a migrated spine plus the shipped router serving the registration surface under a
// verified scope, for ONE tenant.
type registrationFixture struct {
	url          string
	org, project string
	registry     *extensions.SlackRegistry
}

func newRegistrationFixture(t *testing.T, repo *store.Store, org, project string) *registrationFixture {
	t.Helper()
	registry := extensions.NewSlackRegistry(extensions.New(repo.Spine().Pool()))
	ts := httptest.NewServer(api.NewRouter(
		scopedVerifier{middleware.Scope{Project: project, Principal: "prin_registration"}},
		repo, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithSlackConnections(registry)))
	t.Cleanup(ts.Close)
	return &registrationFixture{url: ts.URL, project: project, registry: registry}
}

func (f *registrationFixture) register(t *testing.T, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, f.url+"/v1/slack-connections", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build the registration request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer component-key-not-a-credential")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/slack-connections: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(raw)
}

func openRegistrationStore(t *testing.T) *store.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	repo, err := store.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return repo
}

// TestSlackRegistrationLandsInTheVerifiedTenantAndRefusesACrossTenantSquat is the whole surface in one
// sequence: a workspace registers under the caller's OWN tenant, becomes visible through the list, and a
// SECOND tenant's attempt to bind the same Slack workspace is REFUSED with a 409 that names nothing about
// the tenant that holds it.
func TestSlackRegistrationLandsInTheVerifiedTenantAndRefusesACrossTenantSquat(t *testing.T) {
	repo := openRegistrationStore(t)
	pool := repo.Spine().Pool()

	orgA, projectA := newID("org"), newID("prj")
	orgB, projectB := newID("org"), newID("prj")
	for _, tenant := range [][2]string{{orgA, projectA}, {orgB, projectB}} {
		exec(t, pool, `INSERT INTO organizations (id) VALUES ($1)`, tenant[0])
		exec(t, pool, `INSERT INTO projects (id, organization_id) VALUES ($1,$2)`, tenant[1], tenant[0])
	}
	team := strings.ToUpper(newID("T"))
	body := fmt.Sprintf(`{"team_id":%q,"bot_user_id":"Ubot","signing_secret_ref":"slack/signing",
		"bot_token_ref":"slack/bot","app_token_ref":"slack/app",
		"default_policy":{"agent_revision_id":"arev_x","principal_id":"prin_x"}}`, team)

	a := newRegistrationFixture(t, repo, orgA, projectA)
	resp, raw := a.register(t, body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d, want 201; %s", resp.StatusCode, raw)
	}
	var created struct{ ID string }
	if err := json.Unmarshal([]byte(raw), &created); err != nil || created.ID == "" {
		t.Fatalf("the registration returned no id: %s", raw)
	}

	// The row is in the CALLER's tenant. Read system-scoped so a wrong tenant would be visible rather than
	// filtered away by RLS — the assertion has to be able to fail.
	var gotOrg, gotProject string
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT organization_id, project_id FROM slack_connections WHERE id=$1`, created.ID).
		Scan(&gotOrg, &gotProject); err != nil {
		t.Fatalf("read the registered connection: %v", err)
	}
	if gotOrg != orgA || gotProject != projectA {
		t.Fatalf("the connection landed in (%s,%s), want the VERIFIED scope (%s,%s)", gotOrg, gotProject, orgA, projectA)
	}
	// And no credential VALUE reached the row: the table has ref columns only.
	var valueColumns int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM information_schema.columns
		  WHERE table_name='slack_connections' AND column_name IN ('signing_secret','bot_token','app_token')`).
		Scan(&valueColumns); err != nil {
		t.Fatalf("scan for value columns: %v", err)
	}
	if valueColumns != 0 {
		t.Fatal("slack_connections has a raw credential column; registration must store secret_ref HANDLES only")
	}

	// The operator can find what they just registered.
	listReq, _ := http.NewRequest(http.MethodGet, a.url+"/v1/slack-connections", nil)
	listReq.Header.Set("Authorization", "Bearer component-key-not-a-credential")
	listResp, err := http.DefaultClient.Do(listReq)
	if err != nil {
		t.Fatalf("GET /v1/slack-connections: %v", err)
	}
	listRaw, _ := io.ReadAll(listResp.Body)
	listResp.Body.Close()
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("list = %d, want 200; %s", listResp.StatusCode, listRaw)
	}
	if !strings.Contains(string(listRaw), created.ID) || !strings.Contains(string(listRaw), team) {
		t.Fatalf("the registered workspace is not in the list: %s", listRaw)
	}
	// A list is a browse surface: no secret-ref handle is rendered on it.
	for _, ref := range []string{"slack/signing", "slack/bot", "slack/app", "signing_secret_ref"} {
		if strings.Contains(string(listRaw), ref) {
			t.Fatalf("the list rendered %q; the browse projection carries no credential handles: %s", ref, listRaw)
		}
	}

	// THE SQUAT: tenant B tries to bind the SAME Slack workspace, which would give the unauthenticated
	// team-keyed resolve two candidates for one workspace (E19 T1's hijack).
	b := newRegistrationFixture(t, repo, orgB, projectB)
	squat, squatRaw := b.register(t, body)
	if squat.StatusCode != http.StatusConflict {
		t.Fatalf("a cross-tenant squat = %d, want 409 — a refused hijack must not read as a transient failure; %s",
			squat.StatusCode, squatRaw)
	}
	for _, leak := range []string{orgA, projectA, created.ID} {
		if strings.Contains(squatRaw, leak) {
			t.Fatalf("the refusal body names the holding tenant (%q): %s — the registering admin must not learn that another customer exists", leak, squatRaw)
		}
	}
	// Nothing was written: the workspace still resolves to exactly one connection.
	var rows int
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT count(*) FROM slack_connections WHERE team_id=$1`, team).Scan(&rows); err != nil {
		t.Fatalf("count connections for the workspace: %v", err)
	}
	if rows != 1 {
		t.Fatalf("%d connections bind the workspace after the squat, want 1 — the refusal must be a refusal, not a warning", rows)
	}
}
