//go:build live

// This file is CASE=secret-rotation-restartless, the E13 Task 3 live smoke (SEC-002/MCI-002): on a running,
// already-migrated control-plane store, a tenant's secret is written and ROTATED entirely through the API's
// engine (internal/identity.SecretStore, the code behind POST /v1/secret-refs), with NO process restart, and
// the SAME running process resolves the new value through the production resolver seam. It proves, live and
// end to end:
//
//  1. RESTART-LESS WRITE: a secret POSTed over the API is resolvable on the very next request — no file edit,
//     no restart. The DB-backed store fronts the env-file bridge (the E09 credential-broker seam is preserved).
//  2. RESTART-LESS ROTATION (SEC-002): a rotate inserts a new version the next Resolve reads immediately, on
//     the same live process.
//  3. TENANT ISOLATION (live): a second tenant resolving the SAME ref name is denied by migration 000031's
//     row-level security — it gets a clean miss, never the other tenant's bytes.
//  4. GENUINELY LIVE STACK: the provisioned tenant then drives a REAL provider-one completion, so the store
//     the secrets round-trip through is the same one serving real runs.
//
// HONEST CEILINGS (named, per the live-tier convention):
//   - AT REST: one process-held master-key AES-256-GCM envelope — no KMS backend, no per-secret data key, no
//     one-operation audience/fence lease ceremony (E13-H, SEC-001/003).
//   - "A RUN USES IT": the resolver SEAM (SecretStore.Resolve, the exact hook main.go's dbSecret calls in
//     front of the four env-file resolvers) is proven restart-less + rotating + RLS-isolated here, and the
//     full write→resolve→rotate→RLS chain is proven against a real DB in the component tier. The causal join
//     of a MODEL spontaneously redeeming an API-provisioned secret inside a tools/call is the
//     CASE=mcp-tool-roundtrip machinery (a sandboxed fixture MCP server); it is not re-driven here.
//   - SINGLE PROVIDER: the completion is provider-one only; the secret write-path is provider-agnostic.
//
// GATED: serialized with every LIVE smoke on the shared :local Docker stack; NOT part of make verify / CI.
// Skips cleanly without creds. The credential and every secret value are opaque — never printed.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"

	"github.com/palgroup/palai/storage"
)

// TestLiveSecretRefRestartlessRotation is CASE=secret-rotation-restartless (see the file ceilings).
func TestLiveSecretRefRestartlessRotation(t *testing.T) {
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

	// The secret store the running process holds: one master-key AES-256-GCM envelope, minted in-process (the
	// live harness never writes the key to disk, so it never surfaces). This is the exact store main.go builds
	// from PALAI_SECRET_MASTER_KEY_FILE and puts in front of the env-file bridge.
	var rawKey [32]byte
	if _, err := rand.Read(rawKey[:]); err != nil {
		t.Fatalf("mint master key: %v", err)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(rawKey[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}
	idstore := identity.New(pool)
	secretStore := identity.NewSecretStore(pool, key)

	// Provision a brand-new tenant on the LIVE store (the second-tenant-with-no-restart path).
	org := provisionLiveTenant(t, idstore, "live-secret-tenant")
	scope := middleware.Scope{Organization: org.ID}

	const refName = "provider-upstream-token"

	// (1) RESTART-LESS WRITE: POST a secret over the API engine; the projection carries no value.
	created, err := secretStore.CreateSecretRef(ctx, scope, []byte(`{"name":"`+refName+`","value":"sk-live-secret-v1"}`))
	if err != nil {
		t.Fatalf("CreateSecretRef error = %v", err)
	}
	if body := string(created.Body); strings.Contains(body, "sk-live-secret-v1") || strings.Contains(body, `"value"`) {
		t.Fatalf("create projection disclosed the value: %s", body)
	}
	// The SAME running process resolves it on the very next request — no restart.
	if got, ok, err := secretStore.Resolve(ctx, org.ID, refName); err != nil || !ok || string(got) != "sk-live-secret-v1" {
		t.Fatalf("Resolve(v1) = (%q, ok=%v, err=%v), want (sk-live-secret-v1, true, nil) — restart-less write failed", got, ok, err)
	}

	// (2) RESTART-LESS ROTATION (SEC-002): rotate inserts a new version the next Resolve reads immediately.
	if _, err := secretStore.RotateSecretRef(ctx, scope, refName, []byte(`{"value":"sk-live-secret-v2"}`)); err != nil {
		t.Fatalf("RotateSecretRef error = %v", err)
	}
	if got, ok, err := secretStore.Resolve(ctx, org.ID, refName); err != nil || !ok || string(got) != "sk-live-secret-v2" {
		t.Fatalf("Resolve after rotate = (%q, ok=%v, err=%v), want (sk-live-secret-v2, ...) — rotation not visible without restart", got, ok, err)
	}

	// (3) TENANT ISOLATION (live): a second tenant resolving the SAME ref name is denied by RLS.
	other := provisionLiveTenant(t, idstore, "live-secret-other")
	if got, ok, err := secretStore.Resolve(ctx, other.ID, refName); err != nil {
		t.Fatalf("Resolve(other) error = %v", err)
	} else if ok {
		t.Fatalf("the second tenant resolved the first tenant's secret (%q) — RLS did not isolate live", got)
	}

	// (4) GENUINELY LIVE STACK: the provisioned tenant drives a REAL provider-one completion.
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, org.ID, org.DefaultProjectID)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		response, org.ID, org.DefaultProjectID, session, encodeJSONString("reply with the single word done."))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'queued')`,
		runID, org.ID, org.DefaultProjectID, session, response)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: liveModel(), Secret: modelbroker.SecretRef("provider-one")})
	if err := orch.ExecuteAttempt(ctx, descriptor(runID, 1)); err != nil {
		t.Fatalf("execute the secret-provisioned tenant's run on the real provider: %v", err)
	}
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, runID); n < 1 {
		t.Fatalf("completed model_requests = %d, want >=1 (the real provider must have answered)", n)
	}

	t.Logf("secret-rotation-restartless PASS: tenant %s wrote + ROTATED a secret through the API with NO restart, the same live process resolved each version, a second tenant was RLS-denied the ref, and the tenant ran a REAL provider completion (run %s).", org.ID, runID)
}

// provisionLiveTenant opens a new tenant through the identity store's cross-tenant creation path (the engine
// behind POST /v1/organizations) and returns its ids. The admin key plaintext is discarded — this smoke
// scopes by the provisioned org id directly.
func provisionLiveTenant(t *testing.T, idstore *identity.Store, name string) struct{ ID, DefaultProjectID string } {
	t.Helper()
	created, err := idstore.CreateOrganization(context.Background(), middleware.Scope{}, []byte(`{"display_name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("CreateOrganization(%s) error = %v", name, err)
	}
	var org struct {
		ID               string `json:"id"`
		DefaultProjectID string `json:"default_project_id"`
	}
	if err := json.Unmarshal(created.Body, &org); err != nil {
		t.Fatalf("decode organization body: %v", err)
	}
	if org.ID == "" || org.DefaultProjectID == "" {
		t.Fatalf("provisioned organization is incomplete: %s", created.Body)
	}
	return struct{ ID, DefaultProjectID string }{org.ID, org.DefaultProjectID}
}
