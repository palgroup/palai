package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// modelRouteTestServer builds a router carrying only the options under test, so an unmounted surface is
// observably unmounted.
func modelRouteTestServer(t *testing.T, verifier middleware.Verifier, opts ...RouterOption) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, opts...))
	t.Cleanup(srv.Close)
	return srv.URL
}

// fakeModelRoutes scripts each seam outcome so the model-route handler contract is exercised without a
// database. The recorded scope/body/ids let a test assert the scope came from the verified identity and
// that the path ids reached the store.
type fakeModelRoutes struct {
	out       ProvisionResult
	lastScope middleware.Scope
	lastBody  []byte
	lastRoute string
	lastRev   string
	lastConn  string
}

func (f *fakeModelRoutes) CreateModelConnection(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.out, nil
}
func (f *fakeModelRoutes) CreateModelRoute(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody = s, b
	return f.out, nil
}
func (f *fakeModelRoutes) CreateModelRouteRevision(_ context.Context, s middleware.Scope, routeID string, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastRoute, f.lastBody = s, routeID, b
	return f.out, nil
}
func (f *fakeModelRoutes) PublishModelRouteRevision(_ context.Context, s middleware.Scope, routeID, revisionID string) (ProvisionResult, error) {
	f.lastScope, f.lastRoute, f.lastRev = s, routeID, revisionID
	return f.out, nil
}
func (f *fakeModelRoutes) ListModelConnections(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.out, nil
}
func (f *fakeModelRoutes) GetModelConnection(_ context.Context, s middleware.Scope, connectionID string) (ProvisionResult, error) {
	f.lastScope, f.lastConn = s, connectionID
	return f.out, nil
}
func (f *fakeModelRoutes) ListModelRoutes(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.out, nil
}
func (f *fakeModelRoutes) GetModelRoute(_ context.Context, s middleware.Scope, routeID string) (ProvisionResult, error) {
	f.lastScope, f.lastRoute = s, routeID
	return f.out, nil
}
func (f *fakeModelRoutes) ListModelRouteRevisions(_ context.Context, s middleware.Scope, routeID string) (ProvisionResult, error) {
	f.lastScope, f.lastRoute = s, routeID
	return f.out, nil
}
func (f *fakeModelRoutes) GetModelRouteRevision(_ context.Context, s middleware.Scope, routeID, revisionID string) (ProvisionResult, error) {
	f.lastScope, f.lastRoute, f.lastRev = s, routeID, revisionID
	return f.out, nil
}

// TestModelRouteSurface pins the routing + rendering contract of the E13 T8 write surface: creates are
// 201, a publish is 200, a strict-decode reject is 400, and a route id the caller cannot see is a
// NON-DISCLOSING 404 (never a 403 — a 403 would confirm the id exists in another tenant).
func TestModelRouteSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeModelRoutes{out: ProvisionResult{Body: []byte(`{"id":"mroute_1","object":"model_route"}`)}}
	srv := modelRouteTestServer(t, admin, WithModelRoutes(fake))

	cases := []struct {
		method, path, body string
		wantStatus         int
	}{
		{"POST", "/v1/model-connections", `{"provider":"provider-one","secret_ref":"openai-a"}`, http.StatusCreated},
		{"POST", "/v1/model-routes", `{"name":"default"}`, http.StatusCreated},
		{"POST", "/v1/model-routes/mroute_1/revisions", `{"model":"gpt-4o-mini","connection_id":"mconn_1"}`, http.StatusCreated},
	}
	for _, c := range cases {
		resp := do(t, c.method, srv+c.path, c.body, nil)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", c.method, c.path, resp.StatusCode, c.wantStatus)
		}
		resp.Body.Close()
	}

	resp := do(t, "POST", srv+"/v1/model-routes/mroute_1/revisions/mrev_1/publish", ``, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("publish status = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.lastRoute != "mroute_1" || fake.lastRev != "mrev_1" {
		t.Fatalf("publish reached the store with (%q, %q), want (mroute_1, mrev_1)", fake.lastRoute, fake.lastRev)
	}
	if fake.lastScope.Organization != "org_1" || fake.lastScope.Project != "prj_1" {
		t.Fatalf("store scope = %+v, want the verified identity's org/project (never a body field)", fake.lastScope)
	}

	fake.out = ProvisionResult{BadField: true}
	if resp := do(t, "POST", srv+"/v1/model-connections", `{"secret_value":"sk-live"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported-field create status = %d, want 400", resp.StatusCode)
	}
	fake.out = ProvisionResult{MissingField: "provider"}
	if resp := do(t, "POST", srv+"/v1/model-connections", `{}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field create status = %d, want 400", resp.StatusCode)
	}
	fake.out = ProvisionResult{NotFound: true}
	for _, path := range []string{"/v1/model-routes/foreign/revisions", "/v1/model-routes/foreign/revisions/mrev_x/publish"} {
		resp := do(t, "POST", srv+path, `{"model":"m","connection_id":"c"}`, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("POST %s status = %d, want a non-disclosing 404", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestModelRouteSurfaceRequiresProvisionCapability proves model routing is an org-admin operation: a key
// without the provision capability is refused, and an unauthenticated request never reaches the store.
func TestModelRouteSurfaceRequiresProvisionCapability(t *testing.T) {
	limited := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"responses"}}}
	fake := &fakeModelRoutes{out: ProvisionResult{Body: []byte(`{}`)}}
	srv := modelRouteTestServer(t, limited, WithModelRoutes(fake))

	resp := do(t, "POST", srv+"/v1/model-routes", `{"name":"default"}`, nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("limited-key create status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.lastBody != nil {
		t.Fatal("a key without the provision capability reached the store")
	}
}

// TestModelRouteReadSurface pins the E16 T1 read-back contract (the E13 T10 write-only gap): every
// connection/route/revision is readable via GET(one) + LIST, each read reaches the store carrying the
// VERIFIED identity's scope (never a body/query field), a LIST renders 200, and an id the caller cannot
// see is a NON-DISCLOSING 404 (the store's NotFound, never a 403 that would confirm the id exists).
func TestModelRouteReadSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeModelRoutes{out: ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}}
	srv := modelRouteTestServer(t, admin, WithModelRoutes(fake))

	// Every read is a GET; the LISTs return the ListView envelope, the GETs a single projection.
	lists := []string{
		"/v1/model-connections",
		"/v1/model-routes",
		"/v1/model-routes/mroute_1/revisions",
	}
	for _, path := range lists {
		resp := do(t, "GET", srv+path, "", nil)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s (list) status = %d, want 200", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if fake.lastScope.Organization != "org_1" || fake.lastScope.Project != "prj_1" {
		t.Fatalf("a list reached the store with scope %+v, want the verified identity's org/project", fake.lastScope)
	}

	// Singular GETs carry the path ids through to the store.
	fake.out = ProvisionResult{Body: []byte(`{"id":"mconn_1","object":"model_connection"}`)}
	if resp := do(t, "GET", srv+"/v1/model-connections/mconn_1", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET connection status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if fake.lastConn != "mconn_1" {
		t.Fatalf("GET connection reached the store with %q, want mconn_1", fake.lastConn)
	}
	if resp := do(t, "GET", srv+"/v1/model-routes/mroute_1", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET route status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if resp := do(t, "GET", srv+"/v1/model-routes/mroute_1/revisions/mrev_1", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET revision status = %d, want 200", resp.StatusCode)
	} else {
		resp.Body.Close()
	}
	if fake.lastRoute != "mroute_1" || fake.lastRev != "mrev_1" {
		t.Fatalf("GET revision reached the store with (%q, %q), want (mroute_1, mrev_1)", fake.lastRoute, fake.lastRev)
	}

	// An id owned by another tenant (or absent) is the SAME non-disclosing 404 for every singular GET.
	fake.out = ProvisionResult{NotFound: true}
	for _, path := range []string{"/v1/model-connections/foreign", "/v1/model-routes/foreign", "/v1/model-routes/mroute_1/revisions/foreign"} {
		resp := do(t, "GET", srv+path, "", nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want a non-disclosing 404", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
}

// TestModelRouteReadsRequireProvisionCapability proves a read is an org-admin operation too: a key without
// the provision capability is refused BEFORE the store is reached (a read must not leak another tenant's
// routing to a run-only key).
func TestModelRouteReadsRequireProvisionCapability(t *testing.T) {
	limited := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"responses"}}}
	fake := &fakeModelRoutes{out: ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}}
	srv := modelRouteTestServer(t, limited, WithModelRoutes(fake))

	resp := do(t, "GET", srv+"/v1/model-routes", "", nil)
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("limited-key list status = %d, want 403", resp.StatusCode)
	}
	resp.Body.Close()
	if fake.lastScope.Organization != "" {
		t.Fatal("a key without the provision capability reached the store on a read")
	}
}

// TestModelRoutesUnmountedWithoutOption proves the seam is opt-in: a router built without the option
// serves no model-route path at all (the tiers that never touch routing stay bit-unchanged).
func TestModelRoutesUnmountedWithoutOption(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	srv := modelRouteTestServer(t, admin)
	resp := do(t, "POST", srv+"/v1/model-routes", `{"name":"default"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unmounted model-route status = %d, want 404", resp.StatusCode)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "model_route") {
		t.Fatal("an unmounted surface answered with a model-route body")
	}
	// The read-back routes share the same opt-in gate, so they are unmounted together.
	if r := do(t, "GET", srv+"/v1/model-routes", "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unmounted model-route list status = %d, want 404", r.StatusCode)
	} else {
		r.Body.Close()
	}
}
