//go:build live

// This file is CASE=model-route-per-project, the E13 Task 8 live smoke (MCI-006 / MOD-004 routing half):
// on ONE running control-plane stack, two projects reach the REAL provider with DIFFERENT model ids and
// DIFFERENT credentials — both resolved from the database (model_routes → model_route_revisions →
// model_connections → the E13 T3 secret store), through the production write surface and the production
// resolver seam. It proves, live and end to end:
//
//  1. PER-PROJECT MODEL: project A's run answers on the model A's published route names, project B's on
//     B's — two real chat-completion ids, two different models, one process, no restart.
//  2. PER-PROJECT CREDENTIAL: each run redeems its OWN connection's secret ref, scoped to its own
//     organization. The env deployment default is deliberately set to an UNUSABLE model here, so a run
//     that fell back to it could not have completed — routing is load-bearing for this case to pass.
//  3. THE CREDENTIAL IS REALLY THE ROUTE'S: a third project whose connection names a secret holding an
//     INVALID credential is REJECTED by the real provider, while A and B succeed on the same stack. A
//     shared/ambient credential could not produce that split.
//  4. THE STEP RECORDS ITS TARGET (spec §27.6): each committed model result carries the route revision id
//     that selected it.
//
// HONEST CEILINGS (named, per the live-tier convention):
//   - ONE REAL KEY: only one genuine provider credential is available to this repo, so A's and B's
//     connections are two DISTINCT secret-store entries that hold the SAME upstream key. What is proven
//     live is that each project redeems ITS OWN entry (rung 3 makes a per-project credential observable
//     by rejecting one of them); two commercially distinct provider accounts are NOT claimed.
//   - ONE PROVIDER FAMILY: provider-one only. A second independent adapter and the §27.5 capability probe
//     are E16 — this task decides only which model id and which credential a project uses.
//   - FALLBACK: the deployment-default fallback for an UNROUTED project is proven in the component tier
//     (TestProjectModelRouteRoutesPerProject); here the env default is deliberately unusable.
//   - NO RUN-ROW PIN: the attempt resolves the route once and every step records the revision; pinning the
//     revision on the run row (§27.6's other half) needs a column 000001 does not have.
//
// GATED: serialized with every LIVE smoke on the shared :local Docker stack; NOT part of make verify / CI.
// Skips cleanly without creds. The credential is opaque throughout — it is never printed, never an
// argument, and reaches the database only as an AES-256-GCM sealed blob.
package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/apps/control-plane/api"
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

// liveModelB is the SECOND real model id, so the two projects differ on the wire and not just in the DB.
func liveModelB() string {
	if m := os.Getenv("PALAI_LIVE_MODEL_B"); m != "" {
		return m
	}
	return "gpt-4.1-mini"
}

// TestLiveModelRoutePerProject is CASE=model-route-per-project (see the file ceilings).
func TestLiveModelRoutePerProject(t *testing.T) {
	credential := requireEnv(t, credentialEnv)
	engineDir := requireEnv(t, "PALAI_ENGINE_DIR")
	pgURL := requireEnv(t, "PALAI_COMPONENT_POSTGRES_URL")

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

	// The secret store this process holds — the exact one main.go builds from PALAI_SECRET_MASTER_KEY_FILE
	// and the exact Resolve hook its dbSecret front calls. The master key is minted in-process and never
	// written to disk.
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

	// The broker is production's shape: a tenant-qualified ref (minted by a DB route) redeems from the
	// secret store under its own org; anything else falls back to the env deployment bridge.
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets: execution.RouteSecretResolver{
			// main.go's dbSecret adds a bounded timeout around this same call; the smoke calls the store
			// directly so a resolve failure surfaces as itself rather than as a timeout.
			Lookup:   func(org, name string) ([]byte, bool, error) { return secretStore.Resolve(ctx, org, name) },
			Fallback: modelbroker.EnvResolver{"provider-one": credentialEnv},
		},
	})
	orch := execution.NewOrchestrator(repo, &subprocessDialer{engineDir: engineDir}, broker, toolbroker.New(tools.FileTool()))
	// The deployment default is deliberately UNUSABLE: if a project's run fell back to it instead of
	// routing, the provider would reject the model and the case would fail rather than pass silently.
	orch.SetModelRoute(execution.ModelRoute{Provider: "provider-one", Model: "palai-no-such-deployment-model", Secret: "provider-one"})

	// Two projects, each provisioned through the API's engine, each with its OWN secret entry, connection,
	// route and published revision — written through the production write surface (api.ModelRouteAPI).
	alpha := routeLiveProject(t, ctx, repo, idstore, secretStore, "live-route-alpha", liveModel(), credential)
	beta := routeLiveProject(t, ctx, repo, idstore, secretStore, "live-route-beta", liveModelB(), credential)
	// A third project whose DB credential is INVALID: the real provider must reject it while the other two
	// succeed on this same stack.
	rejected := routeLiveProject(t, ctx, repo, idstore, secretStore, "live-route-rejected", liveModel(), "sk-not-a-real-credential")

	if alpha.model == beta.model {
		t.Fatalf("both projects routed the same model %q — set PALAI_LIVE_MODEL_B to a second real model id", alpha.model)
	}
	if alpha.secretRef == beta.secretRef || alpha.tenant.Organization == beta.tenant.Organization {
		t.Fatal("the two projects must own distinct credential refs in distinct organizations")
	}

	for _, p := range []liveRoutedProject{alpha, beta} {
		runID := seedLiveRun(t, pool, p.tenant)
		if err := orch.ExecuteAttempt(ctx, descriptor(runID, 1)); err != nil {
			t.Fatalf("execute %s's run on the real provider: %v", p.name, err)
		}
		usedModel, revisionID := lastModelResult(t, pool, runID)
		if !strings.HasPrefix(usedModel, p.model) {
			t.Fatalf("%s answered on model %q, want the model its route names (%q)", p.name, usedModel, p.model)
		}
		if revisionID != p.revisionID {
			t.Fatalf("%s's model step recorded route revision %q, want %q (spec §27.6)", p.name, revisionID, p.revisionID)
		}
		if id := lastProviderRequestID(t, pool, p.tenant, runID); id == "" {
			t.Fatalf("%s produced no provider request id — the completion was not real", p.name)
		}
	}

	// The credential really is the route's: this project's run cannot reach the provider, on the very same
	// process that just completed two runs.
	badRun := seedLiveRun(t, pool, rejected.tenant)
	badErr := orch.ExecuteAttempt(ctx, descriptor(badRun, 1))
	if n := countRows(t, pool, `SELECT count(*) FROM model_requests WHERE run_id=$1 AND state='completed'`, badRun); n != 0 {
		t.Fatalf("completed model_requests for the invalid-credential project = %d, want 0 — its run answered on someone's credential", n)
	}
	if badErr == nil {
		t.Fatal("the invalid-credential attempt reported success — a provider rejection must fail the step, not read as an empty answer")
	}
	if !strings.Contains(badErr.Error(), "provider_error") {
		t.Fatalf("invalid-credential attempt error = %v, want the sanitized provider rejection", badErr)
	}

	t.Logf("model-route-per-project PASS: %s ran on %s and %s on %s — different models, different DB-resolved connections, one stack; a third project's invalid DB credential was rejected by the real provider.",
		alpha.name, alpha.model, beta.name, beta.model)
}

// liveRoutedProject is one provisioned tenant with a published model route.
type liveRoutedProject struct {
	name       string
	tenant     coordinator.Tenant
	model      string
	secretRef  string
	revisionID string
}

// routeLiveProject provisions a tenant, stores its provider credential under its OWN org, and publishes a
// model route bound to it — every write through the production API engine (identity.SecretStore and
// api.ModelRouteAPI), never raw SQL.
func routeLiveProject(t *testing.T, ctx context.Context, repo *store.Store, idstore *identity.Store, secrets *identity.SecretStore, name, model, credential string) liveRoutedProject {
	t.Helper()
	org := provisionLiveTenant(t, idstore, name)
	tenant := coordinator.Tenant{Organization: org.ID, Project: org.DefaultProjectID}
	scope := middleware.Scope{Organization: org.ID, Project: org.DefaultProjectID}

	secretRef := name + "-credential"
	// The credential value travels in the request body of the write-only secret API and lands as sealed
	// ciphertext. It is assembled here, never logged; a failure below prints no body.
	body, err := json.Marshal(map[string]string{"name": secretRef, "value": credential})
	if err != nil {
		t.Fatalf("encode secret body: %v", err)
	}
	if _, err := secrets.CreateSecretRef(ctx, scope, body); err != nil {
		t.Fatalf("CreateSecretRef(%s) error = %v", name, err)
	}

	conn, err := repo.CreateModelConnection(ctx, scope, []byte(`{"provider":"provider-one","secret_ref":"`+secretRef+`"}`))
	connID := createdID(t, conn, err)
	route, err := repo.CreateModelRoute(ctx, scope, []byte(`{"name":"`+coordinator.DefaultModelRouteAlias+`"}`))
	routeID := createdID(t, route, err)
	rev, err := repo.CreateModelRouteRevision(ctx, scope, routeID, []byte(`{"model":"`+model+`","connection_id":"`+connID+`"}`))
	revID := createdID(t, rev, err)
	if _, err := repo.PublishModelRouteRevision(ctx, scope, routeID, revID); err != nil {
		t.Fatalf("PublishModelRouteRevision(%s) error = %v", name, err)
	}
	return liveRoutedProject{name: name, tenant: tenant, model: model, secretRef: secretRef, revisionID: revID}
}

// createdID fails the test on a rejected management write and returns the created resource's id.
func createdID(t *testing.T, out api.ProvisionResult, err error) string {
	t.Helper()
	if err != nil {
		t.Fatalf("model-routing write error = %v", err)
	}
	if out.NotFound || out.BadField || out.MissingField != "" {
		t.Fatalf("model-routing write rejected: %+v", out)
	}
	var v struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(out.Body, &v); err != nil || v.ID == "" {
		t.Fatalf("decode id from %s: %v", out.Body, err)
	}
	return v.ID
}

// seedLiveRun creates the session/response/run rows one attempt executes. The run configures no tools, so
// it is a single real model step.
func seedLiveRun(t *testing.T, pool *pgxpool.Pool, tenant coordinator.Tenant) string {
	t.Helper()
	ctx := storage.WithSystemScope(context.Background())
	session, response, runID := newID("ses"), newID("resp"), newID("run")
	do := func(sql string, args ...any) {
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed exec %q: %v", sql, err)
		}
	}
	do(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, session, tenant.Organization, tenant.Project)
	do(`INSERT INTO responses (id, organization_id, project_id, session_id, state, input) VALUES ($1,$2,$3,$4,'queued',$5)`,
		response, tenant.Organization, tenant.Project, session, encodeJSONString("reply with the single word done."))
	do(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'queued')`,
		runID, tenant.Organization, tenant.Project, session, response)
	return runID
}

// lastModelResult reads the newest committed model result's model id and the route revision that step
// recorded (spec §27.6).
func lastModelResult(t *testing.T, pool *pgxpool.Pool, runID string) (string, string) {
	t.Helper()
	var result []byte
	if err := pool.QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT result FROM model_requests WHERE run_id=$1 AND state='completed' ORDER BY updated_at DESC LIMIT 1`,
		runID).Scan(&result); err != nil {
		t.Fatalf("read model result: %v", err)
	}
	var body struct {
		Model           string `json:"model"`
		RouteRevisionID string `json:"route_revision_id"`
	}
	_ = json.Unmarshal(result, &body)
	return body.Model, body.RouteRevisionID
}
