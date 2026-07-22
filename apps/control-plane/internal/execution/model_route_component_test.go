//go:build component

package execution

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/models/fake"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"

	"github.com/palgroup/palai/storage"
)

// seedRoutedProject opens a project on the spine and, when model is non-empty, gives it a PUBLISHED
// model route bound to its own connection (its own credential ref).
func seedRoutedProject(t *testing.T, cs *coordinator.Store, exec func(string, ...any), model, secretRef string) coordinator.Tenant {
	t.Helper()
	tenant := coordinator.Tenant{Organization: pinnedID("org"), Project: pinnedID("prj")}
	exec(`INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	if model == "" {
		return tenant
	}
	ctx := context.Background()
	connID, err := cs.CreateModelConnection(ctx, tenant, "provider-one", secretRef)
	if err != nil {
		t.Fatalf("CreateModelConnection: %v", err)
	}
	routeID, err := cs.CreateModelRoute(ctx, tenant, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute: %v", err)
	}
	rev, err := cs.CreateModelRouteRevision(ctx, tenant, routeID, model, connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision: %v", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, tenant, routeID, rev.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision: %v", err)
	}
	return tenant
}

// TestProjectModelRouteRoutesPerProject is the E13 T8 dispatch contract on ONE stack (MCI-006):
// two projects with their own published routes resolve DIFFERENT models and DIFFERENT credential refs from
// the DB, and a third project with no route still falls back to the env deployment default — the env route
// is demoted to a fallback, not removed. The credential is a tenant-qualified REF at every step; no
// credential value exists on this path.
func TestProjectModelRouteRoutesPerProject(t *testing.T) {
	cs, _, exec := openPinnedSpine(t)
	ctx := context.Background()

	envRoute := ModelRoute{Provider: "fake", Model: "env-default", Secret: "fake"}
	orch := &Orchestrator{spine: cs, route: envRoute}

	projectA := seedRoutedProject(t, cs, exec, "model-alpha", "openai-a")
	projectB := seedRoutedProject(t, cs, exec, "model-beta", "openai-b")
	unrouted := seedRoutedProject(t, cs, exec, "", "")

	state := func(tenant coordinator.Tenant) *attemptState {
		sessionID, runID := pinnedID("ses"), pinnedID("run")
		exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
		exec(`INSERT INTO runs (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'running')`,
			runID, tenant.Organization, tenant.Project, sessionID)
		return &attemptState{
			attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att"))},
			tenant:    tenant,
			sessionID: sessionID,
		}
	}
	stA, stB, stNone := state(projectA), state(projectB), state(unrouted)

	routeA, err := orch.effectiveRoute(ctx, stA)
	if err != nil {
		t.Fatalf("effectiveRoute(A): %v", err)
	}
	routeB, err := orch.effectiveRoute(ctx, stB)
	if err != nil {
		t.Fatalf("effectiveRoute(B): %v", err)
	}
	if routeA.Model != "model-alpha" || routeB.Model != "model-beta" {
		t.Fatalf("routed models = (%q, %q), want (model-alpha, model-beta) — each project must route its own model", routeA.Model, routeB.Model)
	}
	if routeA.Secret == routeB.Secret {
		t.Fatalf("both projects redeemed the SAME credential ref %q — a per-project route must carry a per-project credential", routeA.Secret)
	}
	if routeA.Secret != TenantSecretRef(projectA.Organization, "openai-a") {
		t.Fatalf("project A ref = %q, want the tenant-qualified handle for its own connection", routeA.Secret)
	}
	if routeA.RevisionID == "" || routeB.RevisionID == "" {
		t.Fatal("a DB-resolved route must carry the revision id it selected (spec §27.6)")
	}

	// The unrouted project still runs: the env deployment default is the fallback beneath the route layer.
	routeNone, err := orch.effectiveRoute(ctx, stNone)
	if err != nil {
		t.Fatalf("effectiveRoute(unrouted): %v", err)
	}
	if routeNone.Model != envRoute.Model || routeNone.Secret != envRoute.Secret || routeNone.RevisionID != "" {
		t.Fatalf("unrouted project resolved %+v, want the env deployment default %+v with no revision", routeNone, envRoute)
	}

	// The per-step effective model and the checkpointed config hash both follow the route.
	if model, err := orch.effectiveModel(ctx, stA); err != nil || model != "model-alpha" {
		t.Fatalf("effectiveModel(A) = (%q, %v), want model-alpha", model, err)
	}
	if model, err := orch.effectiveModel(ctx, stNone); err != nil || model != "env-default" {
		t.Fatalf("effectiveModel(unrouted) = (%q, %v), want the env default", model, err)
	}
	hashA, err := orch.effectiveConfigHash(ctx, stA)
	if err != nil {
		t.Fatalf("effectiveConfigHash(A): %v", err)
	}
	hashB, err := orch.effectiveConfigHash(ctx, stB)
	if err != nil {
		t.Fatalf("effectiveConfigHash(B): %v", err)
	}
	if hashA == hashB {
		t.Fatal("two projects on different models + credentials produced the SAME config address")
	}
}

// TestProviderErrorFailsTheModelStep pins the failure mode E13 T8's live smoke uncovered: a provider-side
// rejection (a 401 from a wrong credential, a 429, a 5xx) rides on the RESULT as a sanitized
// modelbroker.Result.Error, NOT as a Go error. Nothing read that field, so such a call was committed as a
// successful step with empty output — the run answered nothing and claimed success.
//
// With per-project credentials a rejected credential is an ordinary tenant-caused condition, so the step
// must FAIL: no committed result, no model.result delivered to the engine, and an error naming the
// sanitized provider code (never the credential).
func TestProviderErrorFailsTheModelStep(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()
	sessionID, responseID, runID := pinnedID("ses"), pinnedID("resp"), pinnedID("run")
	exec(`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, sessionID, tenant.Organization, tenant.Project)
	exec(`INSERT INTO responses (id, organization_id, project_id, session_id, state) VALUES ($1,$2,$3,$4,'queued')`,
		responseID, tenant.Organization, tenant.Project, sessionID)
	exec(`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state) VALUES ($1,$2,$3,$4,$5,'running')`,
		runID, tenant.Organization, tenant.Project, sessionID, responseID)

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"fake": fake.Adapter{Script: fake.Script{
			Model: "fake", Err: &modelbroker.SanitizedError{Code: "provider_error", Message: "upstream declined", Status: 401},
		}}},
		Secrets: modelbroker.StaticResolver{"fake": "unused"},
	})
	ch := &recordingChannel{}
	orch := &Orchestrator{spine: cs, models: broker, route: ModelRoute{Provider: "fake", Model: "fake", Secret: "fake"}}
	st := &attemptState{
		attempt:    AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:     tenant,
		sessionID:  sessionID,
		responseID: responseID,
		ch:         ch,
	}
	requestID := pinnedID("mreq")
	frame := contracts.EngineFrame{Type: "model.request", Data: map[string]any{"model_request_id": requestID, "messages": []any{}}}

	_, err := orch.dispatchModel(ctx, st, frame)
	if err == nil {
		t.Fatal("a provider-rejected call completed the model step — a wrong credential would look like an empty answer")
	}
	if !strings.Contains(err.Error(), "provider_error") {
		t.Fatalf("dispatch error = %v, want it to name the sanitized provider error", err)
	}
	for _, f := range ch.sent {
		if f.Type == "model.result" {
			t.Fatalf("a model.result was delivered for a provider-rejected call: %+v", f.Data)
		}
	}
	var state string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT state FROM model_requests WHERE id=$1`, requestID).Scan(&state); err != nil {
		t.Fatalf("read model_request state: %v", err)
	}
	if state == "completed" {
		t.Fatal("the model_request was committed as completed although the provider rejected the call")
	}
}
