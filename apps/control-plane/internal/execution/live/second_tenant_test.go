//go:build live

// This file is CASE=second-tenant-provisioning, the E13 Task 2 live smoke (MCI-001/TEN-003): a running
// control-plane store — already migrated and bootstrapped — provisions a BRAND-NEW tenant entirely through
// the API's engine (internal/identity, the code behind POST /v1/organizations, /v1/projects PATCH), with NO
// process restart, and that fresh tenant immediately drives a REAL provider-one completion. It proves two
// things end to end:
//
//  1. NO RESTART: the second organization → default project → admin key is minted on the live store, and the
//     admin key resolves through the production credential path (repo.VerifyAPIKey) to the new tenant's scope.
//  2. config_policy IS REACHABLE: a config_policy written through the PATCH write-path (UpdateProjectPolicy)
//     is read back by the coordinator's §14 resolver (ProjectConfig) — the exact path a run's config
//     resolution consults — and only THEN does the tenant run a real completion.
//
// HONEST CEILINGS:
//   - SINGLE PROVIDER: the completion is provider-one only; provisioning is provider-agnostic.
//   - BASIC SCOPES: the admin key is full-capability; named roles/relationships/OIDC are E13-H/E17.
//   - The provisioning is driven through the identity store methods the HTTP handlers call — the HTTP
//     transport around them adds nothing to the "no restart" / "resolver reachable" proof.
//
// GATED: serialized with every LIVE smoke on the shared :local Docker stack; NOT part of make verify / CI.
// Skips cleanly without creds. The credential is an opaque env-resolved secret, never printed.
package live

import (
	"context"
	"encoding/json"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveSecondTenantProvisionedViaAPI is CASE=second-tenant-provisioning (see the file ceilings).
func TestLiveSecondTenantProvisionedViaAPI(t *testing.T) {
	secret := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")
	_ = secret // resolved through the env secret resolver; never referenced directly

	ctx := context.Background()
	repo, err := store.Open(ctx, pgURL)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(repo.Close)
	if err := repo.Migrate(ctx); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	pool := repo.Spine().Pool()

	// Provision a brand-new tenant on the LIVE store — the second-tenant-with-no-restart path.
	idstore := identity.New(pool)
	created, err := idstore.CreateProject(ctx, middleware.Scope{}, []byte(`{"display_name":"live-tenant"}`))
	if err != nil {
		t.Fatalf("CreateProject error = %v", err)
	}
	// THE PROJECT IS THE TENANT NOW. CreateProject seeds the project, its principal, its admin key and its
	// pool in one transaction, so there is no separate organization id and no `default_project_id` — the
	// created row's own `id` is what every scope below keys on.
	var org struct {
		ID          string `json:"id"`
		AdminAPIKey struct {
			Key string `json:"key"`
		} `json:"admin_api_key"`
	}
	if err := json.Unmarshal(created.Body, &org); err != nil {
		t.Fatalf("decode project body: %v", err)
	}
	if org.ID == "" || org.AdminAPIKey.Key == "" {
		t.Fatalf("provisioned project is incomplete: %s", created.Body)
	}

	// Write a config_policy through the PATCH write-path and prove the §14 resolver reads it back.
	adminScope := middleware.Scope{Project: org.ID}
	if _, err := idstore.UpdateProjectPolicy(ctx, adminScope, org.ID,
		[]byte(`{"config_policy":{"default_tools":["file"]}}`)); err != nil {
		t.Fatalf("UpdateProjectPolicy error = %v", err)
	}
	policy, err := repo.Spine().ProjectConfig(ctx, coordinator.Tenant{Project: org.ID})
	if err != nil {
		t.Fatalf("ProjectConfig error = %v", err)
	}
	if len(policy.DefaultTools) != 1 || policy.DefaultTools[0] != "file" {
		t.Fatalf("resolver DefaultTools = %v, want [file] — the config_policy write-path is not reachable", policy.DefaultTools)
	}

	// The admin key resolves through the production credential path to the newly provisioned tenant.
	scope, err := repo.VerifyAPIKey(ctx, org.AdminAPIKey.Key)
	if err != nil {
		t.Fatalf("VerifyAPIKey(new admin key) error = %v", err)
	}
	if scope.Project != org.ID {
		t.Fatalf("admin key resolved to project %s, want %s", scope.Project, org.ID)
	}

	// Seed a queued run for the freshly provisioned tenant and drive it to a REAL completion.
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, org.ID)
	do(`INSERT INTO responses (id, project_id, session_id, state, input) VALUES ($1,$2,$3,'queued',$4)`,
		response, org.ID, session, encodeJSONString("reply with the single word done."))
	do(`INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		runID, org.ID, session, response)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runID, 1)); err != nil {
		t.Fatalf("execute the provisioned tenant's run on the real provider: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, runID); n < 1 {
		t.Fatalf("completed model_requests for the new tenant = %d, want >=1 (the real provider must have answered)", n)
	}

	t.Logf("second-tenant-provisioning PASS: tenant %s provisioned via the API with NO restart, its config_policy is visible in the §14 resolver, and it ran a REAL provider completion (run %s).", org.ID, runID)
}
