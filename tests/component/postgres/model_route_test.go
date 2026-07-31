//go:build component

package postgres

import (
	"context"
	"errors"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"

	"github.com/palgroup/palai/storage"
)

// seedProject opens a second tenant on the same spine (the two-project shape the whole feature exists
// for: one stack, two projects, different model + different credential).
func seedProject(t *testing.T, cs *coordinator.Store) coordinator.Tenant {
	t.Helper()
	tenant := coordinator.Tenant{Organization: newID("org"), Project: newID("prj")}
	exec(t, cs.Pool(), `INSERT INTO organizations (id) VALUES ($1)`, tenant.Organization)
	exec(t, cs.Pool(), `INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, tenant.Project, tenant.Organization)
	return tenant
}

// TestProjectModelRouteResolvesPublishedRevisionOnly is the E13 T8 reader contract against the real
// schema (the FIRST reader of 000001's model_routes/model_route_revisions/model_connections):
//
//   - a project with no route resolves nothing (the caller then falls back to the deployment default);
//   - a DRAFT revision is never routed — an unpublished route must not steer a run;
//   - a PUBLISHED revision routes its model id and its connection's provider + secret REF (never a value);
//   - a later published revision supersedes an earlier one (highest revision wins, deterministically);
//   - a revision naming a connection that does not resolve in this tenant is a LOUD error, never a silent
//     fall-through to the deployment credential (spec §27.7: a route cannot silently select).
func TestProjectModelRouteResolvesPublishedRevisionOnly(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant := seedProject(t, cs)

	if _, found, err := cs.ProjectModelRoute(ctx, tenant); err != nil || found {
		t.Fatalf("ProjectModelRoute(no route) = (found=%v, err=%v), want (false, nil)", found, err)
	}

	connID, err := cs.CreateModelConnection(ctx, tenant, "provider-one", "openai-project-a", "")
	if err != nil {
		t.Fatalf("CreateModelConnection() error = %v", err)
	}
	routeID, err := cs.CreateModelRoute(ctx, tenant, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute() error = %v", err)
	}
	draft, err := cs.CreateModelRouteRevision(ctx, tenant, routeID, "model-draft", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision() error = %v", err)
	}
	if _, found, err := cs.ProjectModelRoute(ctx, tenant); err != nil || found {
		t.Fatalf("ProjectModelRoute(draft only) = (found=%v, err=%v), want (false, nil) — a draft must not route", found, err)
	}

	if err := cs.PublishModelRouteRevision(ctx, tenant, routeID, draft.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision() error = %v", err)
	}
	target, found, err := cs.ProjectModelRoute(ctx, tenant)
	if err != nil || !found {
		t.Fatalf("ProjectModelRoute(published) = (found=%v, err=%v), want (true, nil)", found, err)
	}
	if target.Model != "model-draft" || target.Provider != "provider-one" || target.SecretRef != "openai-project-a" {
		t.Fatalf("routed target = %+v, want model-draft over provider-one with ref openai-project-a", target)
	}
	if target.RevisionID != draft.ID || target.Revision != 1 {
		t.Fatalf("routed revision = (%s, %d), want (%s, 1) — the run must record the revision it selected", target.RevisionID, target.Revision, draft.ID)
	}

	// A second published revision supersedes the first (a revise is a NEW revision; the config bytes of
	// the earlier one are never rewritten).
	next, err := cs.CreateModelRouteRevision(ctx, tenant, routeID, "model-next", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision(2) error = %v", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, tenant, routeID, next.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision(2) error = %v", err)
	}
	if target, _, _ := cs.ProjectModelRoute(ctx, tenant); target.Model != "model-next" || target.Revision != 2 {
		t.Fatalf("after revising, routed target = %+v, want revision 2 with model-next", target)
	}
	// Immutability: the superseded revision still carries its ORIGINAL model.
	var storedModel string
	if err := cs.Pool().QueryRow(storage.WithSystemScope(ctx),
		`SELECT config->>'model' FROM model_route_revisions WHERE id=$1`, draft.ID).Scan(&storedModel); err != nil {
		t.Fatalf("read superseded revision: %v", err)
	}
	if storedModel != "model-draft" {
		t.Fatalf("superseded revision model = %q, want model-draft — a published revision's config is immutable", storedModel)
	}

	// A revision naming a connection that does not resolve in this tenant fails LOUDLY.
	orphan := seedProject(t, cs)
	orphanRoute, err := cs.CreateModelRoute(ctx, orphan, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute(orphan) error = %v", err)
	}
	orphanConn, err := cs.CreateModelConnection(ctx, orphan, "provider-one", "openai-orphan", "")
	if err != nil {
		t.Fatalf("CreateModelConnection(orphan) error = %v", err)
	}
	orphanRev, err := cs.CreateModelRouteRevision(ctx, orphan, orphanRoute, "model-orphan", orphanConn)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision(orphan) error = %v", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, orphan, orphanRoute, orphanRev.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision(orphan) error = %v", err)
	}
	exec(t, cs.Pool(), `DELETE FROM model_connections WHERE id=$1`, orphanConn)
	if _, _, err := cs.ProjectModelRoute(ctx, orphan); err == nil {
		t.Fatal("a published route whose connection no longer resolves must error, not silently fall back to the deployment credential")
	}
}

// TestModelRouteReadsAreTenantScoped is the E16 T1 read-back tenant boundary against the real RLS schema:
// the owner reads its own connections/routes/revisions back (LIST + GET, with the published flag derived
// from config), while the intruder — on the same stack — sees NOTHING of the owner's. A foreign id is the
// same answer as a nonexistent one (NotFound), never a leak, and a LIST is an EMPTY set, never the owner's
// rows. This is the read half of the write-only gap E13 T10 flagged.
func TestModelRouteReadsAreTenantScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	owner, intruder := seedProject(t, cs), seedProject(t, cs)

	connID, err := cs.CreateModelConnection(ctx, owner, "provider-one", "openai-owner", "")
	if err != nil {
		t.Fatalf("CreateModelConnection() error = %v", err)
	}
	routeID, err := cs.CreateModelRoute(ctx, owner, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute() error = %v", err)
	}
	published, err := cs.CreateModelRouteRevision(ctx, owner, routeID, "model-owner", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision(published) error = %v", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, owner, routeID, published.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision() error = %v", err)
	}
	draft, err := cs.CreateModelRouteRevision(ctx, owner, routeID, "model-draft", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision(draft) error = %v", err)
	}

	// The owner reads its own rows back — a connection carries the secret REF name, never a value.
	conns, err := cs.ListModelConnections(ctx, owner)
	if err != nil || len(conns) != 1 || conns[0].ID != connID || conns[0].SecretRef != "openai-owner" || conns[0].Provider != "provider-one" {
		t.Fatalf("ListModelConnections(owner) = (%+v, %v), want the one owner connection with ref openai-owner", conns, err)
	}
	if got, err := cs.GetModelConnection(ctx, owner, connID); err != nil || got.SecretRef != "openai-owner" {
		t.Fatalf("GetModelConnection(owner) = (%+v, %v), want the owner connection", got, err)
	}
	routes, err := cs.ListModelRoutes(ctx, owner)
	if err != nil || len(routes) != 1 || routes[0].ID != routeID || routes[0].Name != coordinator.DefaultModelRouteAlias {
		t.Fatalf("ListModelRoutes(owner) = (%+v, %v), want the one owner route", routes, err)
	}
	if got, err := cs.GetModelRoute(ctx, owner, routeID); err != nil || got.Name != coordinator.DefaultModelRouteAlias {
		t.Fatalf("GetModelRoute(owner) = (%+v, %v), want the owner route", got, err)
	}

	// The revision read-back derives the published flag from config: revision 1 published, 2 a draft.
	revs, err := cs.ListModelRouteRevisions(ctx, owner, routeID)
	if err != nil || len(revs) != 2 {
		t.Fatalf("ListModelRouteRevisions(owner) = (%+v, %v), want 2 revisions", revs, err)
	}
	if !revs[0].Published || revs[0].Model != "model-owner" || revs[0].ConnectionID != connID {
		t.Fatalf("revision 1 = %+v, want published model-owner bound to the owner connection", revs[0])
	}
	if revs[1].Published {
		t.Fatalf("revision 2 = %+v, want a DRAFT (published=false) — a draft never reads back as routed", revs[1])
	}
	if got, err := cs.GetModelRouteRevision(ctx, owner, routeID, published.ID); err != nil || !got.Published || got.Model != "model-owner" {
		t.Fatalf("GetModelRouteRevision(owner, published) = (%+v, %v), want the published revision", got, err)
	}
	if got, err := cs.GetModelRouteRevision(ctx, owner, routeID, draft.ID); err != nil || got.Published {
		t.Fatalf("GetModelRouteRevision(owner, draft) = (%+v, %v), want the draft (published=false)", got, err)
	}
	// A revision id absent from the route is ErrModelRouteRevisionNotFound (not a leak, not a panic).
	if _, err := cs.GetModelRouteRevision(ctx, owner, routeID, "mrev_nonexistent"); !errors.Is(err, coordinator.ErrModelRouteRevisionNotFound) {
		t.Fatalf("GetModelRouteRevision(unknown id) error = %v, want ErrModelRouteRevisionNotFound", err)
	}

	// The intruder — same stack, different tenant — sees NONE of the owner's rows. A LIST is EMPTY; a GET
	// of the owner's id is the SAME non-disclosing NotFound as an id that never existed.
	if got, err := cs.ListModelConnections(ctx, intruder); err != nil || len(got) != 0 {
		t.Fatalf("ListModelConnections(intruder) = (%+v, %v), want an EMPTY set — no cross-tenant leak", got, err)
	}
	if _, err := cs.GetModelConnection(ctx, intruder, connID); !errors.Is(err, coordinator.ErrModelConnectionNotFound) {
		t.Fatalf("GetModelConnection(intruder, owner id) error = %v, want ErrModelConnectionNotFound", err)
	}
	if got, err := cs.ListModelRoutes(ctx, intruder); err != nil || len(got) != 0 {
		t.Fatalf("ListModelRoutes(intruder) = (%+v, %v), want an EMPTY set", got, err)
	}
	if _, err := cs.GetModelRoute(ctx, intruder, routeID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("GetModelRoute(intruder, owner id) error = %v, want ErrModelRouteNotFound", err)
	}
	// Listing/reading the owner's revisions through the intruder's scope is a route miss — the intruder
	// cannot even name the route, so it never reaches the revision rows.
	if _, err := cs.ListModelRouteRevisions(ctx, intruder, routeID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("ListModelRouteRevisions(intruder, owner route) error = %v, want ErrModelRouteNotFound", err)
	}
	if _, err := cs.GetModelRouteRevision(ctx, intruder, routeID, published.ID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("GetModelRouteRevision(intruder, owner route) error = %v, want ErrModelRouteNotFound", err)
	}
}

// TestModelRouteWritesAreTenantScoped proves the write surface discloses nothing across tenants: a route
// id belonging to another project is indistinguishable from a nonexistent one (NotFound → 404, never a
// 403), and a revision can never bind a connection the caller cannot see.
func TestModelRouteWritesAreTenantScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	owner, intruder := seedProject(t, cs), seedProject(t, cs)

	connID, err := cs.CreateModelConnection(ctx, owner, "provider-one", "openai-owner", "")
	if err != nil {
		t.Fatalf("CreateModelConnection() error = %v", err)
	}
	routeID, err := cs.CreateModelRoute(ctx, owner, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute() error = %v", err)
	}
	rev, err := cs.CreateModelRouteRevision(ctx, owner, routeID, "model-owner", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision() error = %v", err)
	}

	// The intruder naming the owner's route id gets the SAME answer as for an id that never existed.
	if _, err := cs.CreateModelRouteRevision(ctx, intruder, routeID, "hijack", connID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("cross-tenant revise error = %v, want ErrModelRouteNotFound (non-disclosing 404)", err)
	}
	if _, err := cs.CreateModelRouteRevision(ctx, intruder, "mroute_does_not_exist", "hijack", connID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("unknown-route revise error = %v, want ErrModelRouteNotFound", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, intruder, routeID, rev.ID); !errors.Is(err, coordinator.ErrModelRouteNotFound) {
		t.Fatalf("cross-tenant publish error = %v, want ErrModelRouteNotFound", err)
	}

	// The owner's own route cannot bind a connection from the other tenant either.
	intruderConn, err := cs.CreateModelConnection(ctx, intruder, "provider-one", "openai-intruder", "")
	if err != nil {
		t.Fatalf("CreateModelConnection(intruder) error = %v", err)
	}
	if _, err := cs.CreateModelRouteRevision(ctx, owner, routeID, "steal", intruderConn); !errors.Is(err, coordinator.ErrModelConnectionNotFound) {
		t.Fatalf("foreign-connection revise error = %v, want ErrModelConnectionNotFound", err)
	}

	// The intruder's own project is unrouted — nothing of the owner's leaks into its resolution.
	if _, found, err := cs.ProjectModelRoute(ctx, intruder); err != nil || found {
		t.Fatalf("ProjectModelRoute(intruder) = (found=%v, err=%v), want (false, nil)", found, err)
	}
}

// THE CUSTOM ENDPOINT SURVIVES THE ROUND TRIP, AND THAT IS THE THIRD SHAPE THE PRODUCT PROMISES (E29).
//
// OpenAI and Anthropic need a family and a key; a self-hosted operator's own vLLM/Ollama/gateway needs an
// ADDRESS, and until migration 000049 model_connections had no column for one. The only way to move that
// endpoint was PALAI_OPENAI_COMPATIBLE_BASE_URL — read once at boot into a single adapter value — so the
// endpoint was a property of the DEPLOYMENT: two projects on one stack could not reach two endpoints, and
// nothing an operator could do through the API changed it.
//
// This drives the REAL store against the REAL schema, and asserts the address arrives on the dispatch
// target — which is the seam the orchestrator copies onto modelbroker.Request.BaseURL and the adapter
// dials. Two projects, two endpoints, one stack.
func TestProjectModelRouteCarriesThePerConnectionEndpoint(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()

	publish := func(tenant coordinator.Tenant, provider, secretRef, baseURL, model string) {
		t.Helper()
		connID, err := cs.CreateModelConnection(ctx, tenant, provider, secretRef, baseURL)
		if err != nil {
			t.Fatalf("CreateModelConnection(%s) error = %v", provider, err)
		}
		routeID, err := cs.CreateModelRoute(ctx, tenant, coordinator.DefaultModelRouteAlias)
		if err != nil {
			t.Fatalf("CreateModelRoute() error = %v", err)
		}
		rev, err := cs.CreateModelRouteRevision(ctx, tenant, routeID, model, connID)
		if err != nil {
			t.Fatalf("CreateModelRouteRevision() error = %v", err)
		}
		if err := cs.PublishModelRouteRevision(ctx, tenant, routeID, rev.ID); err != nil {
			t.Fatalf("PublishModelRouteRevision() error = %v", err)
		}
	}

	// Project A: a self-hosted vLLM. Project B: a different one. Same stack.
	a, b := seedProject(t, cs), seedProject(t, cs)
	publish(a, "openai-compatible", "vllm-a", "http://10.0.0.11:8000/v1/chat/completions", "qwen2.5-coder")
	publish(b, "openai-compatible", "vllm-b", "http://10.0.0.12:8000/v1/chat/completions", "llama-3.3")

	targetA, found, err := cs.ProjectModelRoute(ctx, a)
	if err != nil || !found {
		t.Fatalf("ProjectModelRoute(A) = (found=%v, err=%v)", found, err)
	}
	targetB, found, err := cs.ProjectModelRoute(ctx, b)
	if err != nil || !found {
		t.Fatalf("ProjectModelRoute(B) = (found=%v, err=%v)", found, err)
	}
	if targetA.BaseURL != "http://10.0.0.11:8000/v1/chat/completions" {
		t.Fatalf("A's endpoint = %q, want its own — a per-project custom endpoint does not reach dispatch", targetA.BaseURL)
	}
	if targetB.BaseURL == targetA.BaseURL {
		t.Fatalf("both projects resolved the SAME endpoint (%q): the endpoint is still a deployment property", targetA.BaseURL)
	}

	// A family with its own endpoint carries none, so its adapter dials exactly where it did before 000049.
	c := seedProject(t, cs)
	publish(c, "provider-one", "openai-c", "", "gpt-4o-mini")
	targetC, found, err := cs.ProjectModelRoute(ctx, c)
	if err != nil || !found {
		t.Fatalf("ProjectModelRoute(C) = (found=%v, err=%v)", found, err)
	}
	if targetC.BaseURL != "" {
		t.Fatalf("an OpenAI connection resolved endpoint %q, want empty", targetC.BaseURL)
	}
}

// The verification stamp round-trips, and it is scoped to the tenant that owns the row. It gates NOTHING —
// a connection stamped `credential_rejected` still resolves onto its route exactly as before — and that is
// asserted here rather than described, because a cache that quietly became a gate would take runs down.
func TestModelConnectionVerificationStampIsACacheNotAGate(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	tenant, other := seedProject(t, cs), seedProject(t, cs)

	connID, err := cs.CreateModelConnection(ctx, tenant, "provider-one", "openai-a", "")
	if err != nil {
		t.Fatalf("CreateModelConnection() error = %v", err)
	}
	routeID, err := cs.CreateModelRoute(ctx, tenant, coordinator.DefaultModelRouteAlias)
	if err != nil {
		t.Fatalf("CreateModelRoute() error = %v", err)
	}
	rev, err := cs.CreateModelRouteRevision(ctx, tenant, routeID, "gpt-4o-mini", connID)
	if err != nil {
		t.Fatalf("CreateModelRouteRevision() error = %v", err)
	}
	if err := cs.PublishModelRouteRevision(ctx, tenant, routeID, rev.ID); err != nil {
		t.Fatalf("PublishModelRouteRevision() error = %v", err)
	}

	// A connection nobody has probed reads back with no stamp — an absence, not a failure.
	before, err := cs.GetModelConnection(ctx, tenant, connID)
	if err != nil || !before.VerifiedAt.IsZero() || before.VerificationOutcome != "" {
		t.Fatalf("an unprobed connection = (%+v, %v), want no stamp", before, err)
	}

	if err := cs.RecordModelConnectionVerification(ctx, tenant, connID, "credential_rejected"); err != nil {
		t.Fatalf("RecordModelConnectionVerification() error = %v", err)
	}
	after, err := cs.GetModelConnection(ctx, tenant, connID)
	if err != nil || after.VerificationOutcome != "credential_rejected" || after.VerifiedAt.IsZero() {
		t.Fatalf("after a stamp = (%+v, %v), want the outcome AND its age", after, err)
	}

	// THE STAMP GATES NOTHING. A rejected credential still routes — the operator's run fails at the
	// provider with the provider's own error, which is a better message than one this cache could invent.
	if _, found, err := cs.ProjectModelRoute(ctx, tenant); err != nil || !found {
		t.Fatalf("a connection stamped `credential_rejected` stopped resolving (found=%v, err=%v): the "+
			"verification cache has become a gate", found, err)
	}

	// A foreign tenant cannot stamp it, and the refusal is the same NotFound an absent id gets.
	if err := cs.RecordModelConnectionVerification(ctx, other, connID, "credential_accepted"); !errors.Is(err, coordinator.ErrModelConnectionNotFound) {
		t.Fatalf("a foreign stamp = %v, want ErrModelConnectionNotFound", err)
	}
}
