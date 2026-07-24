package store

import (
	"context"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// TestModelRouteWritesRejectAnOrgGranularKey pins the E13 T8 review NIT 2: an ORG-granular provision key
// (the T2 shape — Scope.Project == "") is a LEGITIMATE key, but every model-routing row is keyed by
// (organization, project). Without a guard such a key inserts project_id=” and the composite FK to
// projects rejects it, surfacing as a 500 for a well-formed request. It must be a 400 naming what is
// missing. The guard short-circuits before any query, so this needs no database.
func TestModelRouteWritesRejectAnOrgGranularKey(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	orgOnly := middleware.Scope{Organization: "org_1"}

	conn, err := s.CreateModelConnection(ctx, orgOnly, []byte(`{"provider":"provider-one","secret_ref":"openai"}`))
	if err != nil || conn.MissingField == "" {
		t.Fatalf("CreateModelConnection(org-granular key) = (%+v, %v), want a MissingField reject (400), not a DB error", conn, err)
	}
	route, err := s.CreateModelRoute(ctx, orgOnly, []byte(`{"name":"default"}`))
	if err != nil || route.MissingField == "" {
		t.Fatalf("CreateModelRoute(org-granular key) = (%+v, %v), want a MissingField reject (400)", route, err)
	}
	rev, err := s.CreateModelRouteRevision(ctx, orgOnly, "mroute_1", []byte(`{"model":"m","connection_id":"mconn_1"}`))
	if err != nil || rev.MissingField == "" {
		t.Fatalf("CreateModelRouteRevision(org-granular key) = (%+v, %v), want a MissingField reject (400)", rev, err)
	}
	pub, err := s.PublishModelRouteRevision(ctx, orgOnly, "mroute_1", "mrev_1")
	if err != nil || pub.MissingField == "" {
		t.Fatalf("PublishModelRouteRevision(org-granular key) = (%+v, %v), want a MissingField reject (400)", pub, err)
	}
}

// TestModelRouteReadsRejectAnOrgGranularKey is the read-back partner (E16 T1, review SF-2) of the write
// guard above: the six read-back methods must short-circuit on an ORG-granular key (Scope.Project == "")
// with the same MissingField reject (400) BEFORE any store query — model routing is per project. Without
// requireProjectScope on a read, an org-granular key would reach the (here nil) spine, panicking or 500ing
// a well-formed request. This is the fast glue-layer regression guard partnering the real-Postgres
// tenant-scoping proof (tests/component/postgres TestModelRouteReadsAreTenantScoped); it needs no database.
func TestModelRouteReadsRejectAnOrgGranularKey(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	orgOnly := middleware.Scope{Organization: "org_1"}

	reads := []struct {
		name string
		call func() (api.ProvisionResult, error)
	}{
		{"ListModelConnections", func() (api.ProvisionResult, error) { return s.ListModelConnections(ctx, orgOnly) }},
		{"GetModelConnection", func() (api.ProvisionResult, error) { return s.GetModelConnection(ctx, orgOnly, "mconn_1") }},
		{"ListModelRoutes", func() (api.ProvisionResult, error) { return s.ListModelRoutes(ctx, orgOnly) }},
		{"GetModelRoute", func() (api.ProvisionResult, error) { return s.GetModelRoute(ctx, orgOnly, "mroute_1") }},
		{"ListModelRouteRevisions", func() (api.ProvisionResult, error) { return s.ListModelRouteRevisions(ctx, orgOnly, "mroute_1") }},
		{"GetModelRouteRevision", func() (api.ProvisionResult, error) {
			return s.GetModelRouteRevision(ctx, orgOnly, "mroute_1", "mrev_1")
		}},
	}
	for _, r := range reads {
		out, err := r.call()
		if err != nil || out.MissingField == "" {
			t.Fatalf("%s(org-granular key) = (%+v, %v), want a MissingField reject (400), not a store call", r.name, out, err)
		}
	}
}
