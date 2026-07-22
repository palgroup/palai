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

	connID, err := cs.CreateModelConnection(ctx, tenant, "provider-one", "openai-project-a")
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
	orphanConn, err := cs.CreateModelConnection(ctx, orphan, "provider-one", "openai-orphan")
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

// TestModelRouteWritesAreTenantScoped proves the write surface discloses nothing across tenants: a route
// id belonging to another project is indistinguishable from a nonexistent one (NotFound → 404, never a
// 403), and a revision can never bind a connection the caller cannot see.
func TestModelRouteWritesAreTenantScoped(t *testing.T) {
	cs := openHarness(t)
	ctx := context.Background()
	owner, intruder := seedProject(t, cs), seedProject(t, cs)

	connID, err := cs.CreateModelConnection(ctx, owner, "provider-one", "openai-owner")
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
	intruderConn, err := cs.CreateModelConnection(ctx, intruder, "provider-one", "openai-intruder")
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
