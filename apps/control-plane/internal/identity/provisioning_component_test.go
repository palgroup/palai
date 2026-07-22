//go:build component

// Package identity_test holds the real-PostgreSQL component tests for the tenancy provisioning store. They
// run only under `make test-component TEST=postgres` (which starts a throwaway container and exports
// PALAI_COMPONENT_POSTGRES_URL) — the build tag keeps them out of the credential-free, Docker-free unit tier.
package identity_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// openHarness returns a migrated durable-spine store; Migrate is idempotent so every test starts from
// applied schema. It shares the same throwaway container the postgres component suite uses.
func openHarness(t *testing.T) *coordinator.Store {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run make test-component TEST=postgres")
	}
	cs, err := coordinator.Open(context.Background(), url)
	if err != nil {
		t.Fatalf("coordinator.Open() error = %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(context.Background()); err != nil {
		t.Fatalf("Migrate() error = %v", err)
	}
	return cs
}

func newID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}

// provisionOrg opens a NEW tenant through the identity store's cross-tenant creation path (the engine
// behind POST /v1/organizations) and returns the organization id, its default project id, and the admin
// key's plaintext. It is the "second tenant with no restart" primitive (MCI-001).
func provisionOrg(t *testing.T, idstore *identity.Store, name string) (org, project, plaintext string) {
	t.Helper()
	out, err := idstore.CreateOrganization(context.Background(), middleware.Scope{}, []byte(`{"display_name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("CreateOrganization(%s) error = %v", name, err)
	}
	var r struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
		AdminAPIKey      struct {
			ID  string `json:"id"`
			Key string `json:"key"`
		} `json:"admin_api_key"`
	}
	if err := json.Unmarshal(out.Body, &r); err != nil {
		t.Fatalf("decode organization body: %v", err)
	}
	if r.ID == "" || r.DefaultProjectID == "" || r.AdminAPIKey.Key == "" {
		t.Fatalf("provisioned organization is incomplete: %s", out.Body)
	}
	return r.ID, r.DefaultProjectID, r.AdminAPIKey.Key
}

// TestProvisionSecondTenantViaAPI proves MCI-001/TEN-003: a running store mints a brand-new tenant whose
// admin key resolves through the production credential path, and the two tenants are isolated — one org's
// scope lists only its own projects, never the other's (migration 000029 RLS under the provisioning scope).
func TestProvisionSecondTenantViaAPI(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	aOrg, aProj, aKey := provisionOrg(t, idstore, "alpha")
	bOrg, bProj, bKey := provisionOrg(t, idstore, "beta")
	if aOrg == bOrg {
		t.Fatal("two CreateOrganization calls produced the same organization id")
	}

	scopeA, err := cs.VerifyAPIKey(ctx, aKey)
	if err != nil {
		t.Fatalf("VerifyAPIKey(alpha admin key) error = %v", err)
	}
	if scopeA.Organization != aOrg || scopeA.Project != aProj {
		t.Fatalf("alpha key resolved to (%s,%s), want (%s,%s)", scopeA.Organization, scopeA.Project, aOrg, aProj)
	}
	scopeB, err := cs.VerifyAPIKey(ctx, bKey)
	if err != nil {
		t.Fatalf("VerifyAPIKey(beta admin key) error = %v", err)
	}
	if scopeB.Organization != bOrg {
		t.Fatalf("beta key resolved to org %s, want %s", scopeB.Organization, bOrg)
	}

	// Isolation: listing projects under beta's scope returns beta's default project and NOT alpha's.
	list, err := idstore.ListProjects(ctx, middleware.Scope{Organization: bOrg, Project: bProj})
	if err != nil {
		t.Fatalf("ListProjects(beta) error = %v", err)
	}
	body := string(list.Body)
	if !strings.Contains(body, bProj) {
		t.Fatalf("beta's project list %q is missing its own project %s", body, bProj)
	}
	if strings.Contains(body, aProj) {
		t.Fatalf("beta's project list %q leaked alpha's project %s — RLS did not isolate", body, aProj)
	}
}

// TestConfigPolicyResolverReachable proves the §14 project layer is now API-reachable: a config_policy
// written through UpdateProjectPolicy is read back by the coordinator's resolver, the exact path a run's
// admission/config resolution consults.
func TestConfigPolicyResolverReachable(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	org, proj, _ := provisionOrg(t, idstore, "gamma")
	scope := middleware.Scope{Organization: org, Project: proj}
	if _, err := idstore.UpdateProjectPolicy(ctx, scope, proj,
		[]byte(`{"config_policy":{"allowed_models":["gpt-x"],"default_tools":["file"]}}`)); err != nil {
		t.Fatalf("UpdateProjectPolicy error = %v", err)
	}

	policy, err := cs.ProjectConfig(ctx, coordinator.Tenant{Organization: org, Project: proj})
	if err != nil {
		t.Fatalf("ProjectConfig error = %v", err)
	}
	if len(policy.AllowedModels) != 1 || policy.AllowedModels[0] != "gpt-x" {
		t.Fatalf("resolver AllowedModels = %v, want [gpt-x]", policy.AllowedModels)
	}
	if len(policy.DefaultTools) != 1 || policy.DefaultTools[0] != "file" {
		t.Fatalf("resolver DefaultTools = %v, want [file]", policy.DefaultTools)
	}
}

// TestProvisioningStrictDecodeRejectsUnknownField proves the write-path uses the E11 T1 strict decode: an
// unknown field is a typed reject (400), and a config_policy PATCH with no policy is a missing-field 400.
func TestProvisioningStrictDecodeRejectsUnknownField(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	org, proj, _ := provisionOrg(t, idstore, "delta")
	scope := middleware.Scope{Organization: org, Project: proj}

	if r, _ := idstore.CreateProject(ctx, scope, []byte(`{"nope":1}`)); !r.BadField {
		t.Fatal("CreateProject with an unknown field was not rejected")
	}
	if r, _ := idstore.UpdateProjectPolicy(ctx, scope, proj, []byte(`{"config_policy":{"bad":1}}`)); !r.BadField {
		t.Fatal("UpdateProjectPolicy with an unknown config_policy field was not rejected")
	}
	if r, _ := idstore.UpdateProjectPolicy(ctx, scope, proj, []byte(`{}`)); r.MissingField != "config_policy" {
		t.Fatalf("UpdateProjectPolicy with no policy MissingField = %q, want config_policy", r.MissingField)
	}
}

// TestCreateAPIKeyForForeignProjectDenied proves a key can only be minted for a project in the caller's own
// organization: another tenant's project is invisible under RLS, so the create is a 404 rather than a
// cross-tenant write, while a key for the caller's own project succeeds and carries a plaintext once.
func TestCreateAPIKeyForForeignProjectDenied(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	aOrg, aProj, _ := provisionOrg(t, idstore, "eps-a")
	_, bProj, _ := provisionOrg(t, idstore, "eps-b")
	scopeA := middleware.Scope{Organization: aOrg, Project: aProj}

	if r, _ := idstore.CreateAPIKey(ctx, scopeA, []byte(`{"project_id":"`+bProj+`"}`)); !r.NotFound {
		t.Fatal("minting a key for another tenant's project was not denied")
	}
	own, err := idstore.CreateAPIKey(ctx, scopeA, []byte(`{"project_id":"`+aProj+`","scopes":["run"]}`))
	if err != nil {
		t.Fatalf("CreateAPIKey(own project) error = %v", err)
	}
	if !strings.Contains(string(own.Body), `"key":"sk_`) {
		t.Fatalf("own-project key create %q did not carry a plaintext key", own.Body)
	}
}

// TestListAPIKeysMetadataOnly proves a listing never discloses a secret: neither the plaintext nor the
// stored hash appears, only metadata.
func TestListAPIKeysMetadataOnly(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	org, proj, _ := provisionOrg(t, idstore, "zeta")
	scope := middleware.Scope{Organization: org, Project: proj}
	if _, err := idstore.CreateAPIKey(ctx, scope, []byte(`{"project_id":"`+proj+`"}`)); err != nil {
		t.Fatalf("CreateAPIKey error = %v", err)
	}
	list, err := idstore.ListAPIKeys(ctx, scope)
	if err != nil {
		t.Fatalf("ListAPIKeys error = %v", err)
	}
	body := string(list.Body)
	if strings.Contains(body, "sk_") || strings.Contains(body, `"key"`) || strings.Contains(body, "hash") {
		t.Fatalf("api-key listing disclosed a secret: %s", body)
	}
}

// TestBootstrapFirstOrgResolvable proves the bootstrap narrowing: seeding the first organization through
// the SAME identity provisioning path yields an admin key that resolves via the production credential path.
func TestBootstrapFirstOrgResolvable(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	idstore := identity.New(cs.Pool())

	bootKey := newID("bootsecret")
	if err := idstore.ProvisionFirstOrg(ctx, bootKey); err != nil {
		t.Fatalf("ProvisionFirstOrg error = %v", err)
	}
	scope, err := cs.VerifyAPIKey(ctx, bootKey)
	if err != nil {
		t.Fatalf("VerifyAPIKey(bootstrap key) error = %v", err)
	}
	if scope.Organization == "" || scope.Project == "" {
		t.Fatalf("bootstrap key resolved to an incomplete scope: %+v", scope)
	}
	// Re-seeding is a clean no-op (ON CONFLICT DO NOTHING), so the key still resolves.
	if err := idstore.ProvisionFirstOrg(ctx, bootKey); err != nil {
		t.Fatalf("second ProvisionFirstOrg error = %v", err)
	}
	if _, err := cs.VerifyAPIKey(storage.WithSystemScope(ctx), bootKey); err != nil {
		t.Fatalf("VerifyAPIKey after re-seed error = %v", err)
	}
}
