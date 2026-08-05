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
//  3. TENANT ISOLATION (live): a second tenant resolving the SAME ref name is denied by the row-level
//     security migration 000006 installed — it gets a clean miss, never the other tenant's bytes. This leg
//     ASSERTED THE OPPOSITE for one phase; step (3) in the body carries the whole record of why.
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
	"github.com/palgroup/palai/packages/coordinator"
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
	project := provisionLiveTenant(t, idstore, "live-secret-tenant")
	scope := middleware.Scope{Project: project}
	tenant := coordinator.Tenant{Project: project}

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
	if got, ok, err := secretStore.Resolve(ctx, tenant, refName); err != nil || !ok || string(got) != "sk-live-secret-v1" {
		t.Fatalf("Resolve(v1) = (%q, ok=%v, err=%v), want (sk-live-secret-v1, true, nil) — restart-less write failed", got, ok, err)
	}

	// (2) RESTART-LESS ROTATION (SEC-002): rotate inserts a new version the next Resolve reads immediately.
	if _, err := secretStore.RotateSecretRef(ctx, scope, refName, []byte(`{"value":"sk-live-secret-v2"}`)); err != nil {
		t.Fatalf("RotateSecretRef error = %v", err)
	}
	if got, ok, err := secretStore.Resolve(ctx, tenant, refName); err != nil || !ok || string(got) != "sk-live-secret-v2" {
		t.Fatalf("Resolve after rotate = (%q, ok=%v, err=%v), want (sk-live-secret-v2, ...) — rotation not visible without restart", got, ok, err)
	}

	// (3) THE TENANT BOUNDARY IS BACK, AND THIS LEG HAS NOW ASSERTED IT, ITS ABSENCE, AND IT AGAIN. The
	// full record, because a reader meeting the middle version elsewhere in the tree needs it:
	//
	//   - Before A.2 this proved a second tenant was RLS-denied the first tenant's ref.
	//   - A.2 removed organizations and left secret_refs with NO tenant column, so there was nothing
	//     narrower than the installation for a policy to key on. A.3 T8 INVERTED this leg rather than
	//     deleting it, and was right to: "an absence nobody asserts is an absence that changes without
	//     anyone noticing". Its own words were that if a per-project boundary were ever added back, THIS
	//     test would fail and name the change.
	//   - Migration 000006 adds project_id. This is that failure, paid.
	//
	// AND THE INVERTED VERSION HAD A DEFECT WORTH RECORDING SEPARATELY, because it is a shape this tree
	// keeps finding rather than a typo: it provisioned a second tenant, wrote `_ = other`, and then called
	// `secretStore.Resolve(ctx, refName)` — the signature took no tenant, so THE SECOND TENANT WAS NEVER
	// PART OF THE MEASUREMENT. The leg passed because the first tenant resolved its own secret. A test
	// asserting a cross-tenant fact through an API with no tenant parameter cannot have been measuring one,
	// and nothing in the file said so.
	otherProject := provisionLiveTenant(t, idstore, "live-secret-other")
	otherTenant := coordinator.Tenant{Project: otherProject}
	if got, ok, err := secretStore.Resolve(ctx, otherTenant, refName); err != nil {
		t.Fatalf("Resolve(second tenant) error = %v — a foreign name must be a clean miss, not an error", err)
	} else if ok {
		t.Fatalf("a second tenant resolved (%q) for the first tenant's ref — 000006 keys secret_refs on project_id, so this must be a miss", got)
	}
	// AND THE FIRST TENANT STILL READS ITS OWN, checked after the refusal so a policy that denied everyone
	// could not pass this step. A boundary that also blocks the owner is an outage, not isolation.
	if got, ok, err := secretStore.Resolve(ctx, tenant, refName); err != nil || !ok || string(got) != "sk-live-secret-v2" {
		t.Fatalf("the owning tenant lost its own secret after the boundary check: (%q, ok=%v, err=%v)", got, ok, err)
	}

	// (4) GENUINELY LIVE STACK: the provisioned tenant drives a REAL provider-one completion.
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(storage.WithSystemScope(ctx), sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, project_id) VALUES ($1,$2)`, session, project)
	do(`INSERT INTO responses (id, project_id, session_id, state, input) VALUES ($1,$2,$3,'queued',$4)`,
		response, project, session, encodeJSONString("reply with the single word done."))
	do(`INSERT INTO runs (id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		runID, project, session, response)

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

	// The transcript names what each leg actually measured. It said "a second tenant was RLS-denied the ref"
	// through the entire phase in which step (3) asserted the exact opposite — a PASS line is the thing an
	// operator reads instead of the test, so a stale one ships a false transcript.
	t.Logf("secret-rotation-restartless PASS: tenant %s wrote + ROTATED a secret through the API with NO restart, the same live process resolved each version, a SECOND tenant (%s) was refused that ref while the owner kept it, and the tenant ran a REAL provider completion (run %s).", project, otherProject, runID)
}

// provisionLiveTenant opens a new tenant through the identity store's installation-scoped creation path
// (the engine behind POST /v1/projects) and returns its project id. The admin key plaintext is discarded —
// this smoke scopes by the provisioned project directly.
//
// A.2 MADE THE PROJECT THE TENANT. `CreateOrganization` is gone and `CreateProject` mints the project, its
// principal, its admin key and its pool in one seed, so there is no longer an org id to carry beside the
// project id — the two were always the same tenant wearing two names here.
func provisionLiveTenant(t *testing.T, idstore *identity.Store, name string) string {
	t.Helper()
	created, err := idstore.CreateProject(context.Background(), middleware.Scope{}, []byte(`{"display_name":"`+name+`"}`))
	if err != nil {
		t.Fatalf("CreateProject(%s) error = %v", name, err)
	}
	var project struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(created.Body, &project); err != nil {
		t.Fatalf("decode project body: %v", err)
	}
	if project.ID == "" {
		t.Fatalf("provisioned project is incomplete: %s", created.Body)
	}
	return project.ID
}
