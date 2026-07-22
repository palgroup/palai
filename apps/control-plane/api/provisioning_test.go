package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeProvisioning scripts each seam outcome so the handler contract is exercised without a database. One
// result backs the create ops and another the reads; the recorded scope lets a test assert the gate ran.
type fakeProvisioning struct {
	create     ProvisionResult
	read       ProvisionResult
	lastScope  middleware.Scope
	lastBody   []byte
	lastID     string
	lastMethod string
}

func (f *fakeProvisioning) CreateOrganization(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody, f.lastMethod = s, b, "CreateOrganization"
	return f.create, nil
}
func (f *fakeProvisioning) ListOrganizations(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope, f.lastMethod = s, "ListOrganizations"
	return f.read, nil
}
func (f *fakeProvisioning) GetOrganization(_ context.Context, s middleware.Scope, id string) (ProvisionResult, error) {
	f.lastScope, f.lastID = s, id
	return f.read, nil
}
func (f *fakeProvisioning) CreateProject(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody, f.lastMethod = s, b, "CreateProject"
	return f.create, nil
}
func (f *fakeProvisioning) ListProjects(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeProvisioning) GetProject(_ context.Context, s middleware.Scope, id string) (ProvisionResult, error) {
	f.lastScope, f.lastID = s, id
	return f.read, nil
}
func (f *fakeProvisioning) UpdateProjectPolicy(_ context.Context, s middleware.Scope, id string, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastID, f.lastBody, f.lastMethod = s, id, b, "UpdateProjectPolicy"
	return f.read, nil
}
func (f *fakeProvisioning) CreateAPIKey(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody, f.lastMethod = s, b, "CreateAPIKey"
	return f.create, nil
}
func (f *fakeProvisioning) ListAPIKeys(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope = s
	return f.read, nil
}
func (f *fakeProvisioning) GetAPIKey(_ context.Context, s middleware.Scope, id string) (ProvisionResult, error) {
	f.lastScope, f.lastID = s, id
	return f.read, nil
}
func (f *fakeProvisioning) RevokeAPIKey(_ context.Context, s middleware.Scope, id string) (ProvisionResult, error) {
	f.lastScope, f.lastID, f.lastMethod = s, id, "RevokeAPIKey"
	return f.read, nil
}

// scopedVerifier resolves every bearer to a fixed scope, so a test can drive the provision-capability gate.
type scopedVerifier struct{ scope middleware.Scope }

func (v scopedVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return v.scope, nil
}

func provisioningTestServer(t *testing.T, verifier middleware.Verifier, prov ProvisioningAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, prov, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestProvisioningSurface pins the routing + rendering contract for every provisioning route: creates are
// 201 with the resource-typed Location, reads are 200, a strict-decode reject is a 400, and an
// absent/foreign resource is a 404. An admin key (empty scopes) passes the capability gate.
func TestProvisioningSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1"}}
	fake := &fakeProvisioning{
		create: ProvisionResult{Body: []byte(`{"id":"x_1","object":"resource"}`)},
		read:   ProvisionResult{Body: []byte(`{"object":"resource"}`)},
	}
	base := provisioningTestServer(t, admin, fake)

	cases := []struct {
		method, path, body, wantLoc string
		wantStatus                  int
	}{
		{"POST", "/v1/organizations", `{"display_name":"acme"}`, "/v1/organizations/x_1", http.StatusCreated},
		{"GET", "/v1/organizations", ``, "", http.StatusOK},
		{"GET", "/v1/organizations/org_9", ``, "", http.StatusOK},
		{"POST", "/v1/projects", `{"display_name":"p"}`, "/v1/projects/x_1", http.StatusCreated},
		{"GET", "/v1/projects", ``, "", http.StatusOK},
		{"GET", "/v1/projects/prj_9", ``, "", http.StatusOK},
		{"PATCH", "/v1/projects/prj_9", `{"config_policy":{"allowed_models":["m"]}}`, "", http.StatusOK},
		{"POST", "/v1/api-keys", `{"project_id":"prj_1"}`, "/v1/api-keys/x_1", http.StatusCreated},
		{"GET", "/v1/api-keys", ``, "", http.StatusOK},
		{"GET", "/v1/api-keys/key_9", ``, "", http.StatusOK},
		{"POST", "/v1/api-keys/key_9/revoke", ``, "", http.StatusOK},
	}
	for _, c := range cases {
		resp := do(t, c.method, base+c.path, c.body, nil)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", c.method, c.path, resp.StatusCode, c.wantStatus)
		}
		if c.wantLoc != "" && resp.Header.Get("Location") != c.wantLoc {
			t.Fatalf("%s %s Location = %q, want %q", c.method, c.path, resp.Header.Get("Location"), c.wantLoc)
		}
	}

	// A strict-decode reject renders 400, an absent resource renders 404 — the typed store outcomes.
	fake.create = ProvisionResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/projects", `{"nope":1}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-field create status = %d, want 400", resp.StatusCode)
	}
	fake.create = ProvisionResult{MissingField: "project_id"}
	if resp := do(t, "POST", base+"/v1/api-keys", `{}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field create status = %d, want 400", resp.StatusCode)
	}
	fake.read = ProvisionResult{NotFound: true}
	if resp := do(t, "GET", base+"/v1/projects/prj_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown project status = %d, want 404", resp.StatusCode)
	}
}

// TestProvisioningRequiresProvisionScope proves the basic-scope gate: a key whose non-empty scopes omit
// `provision` is refused (403) on every provisioning route, while an admin key (empty scopes) is admitted.
func TestProvisioningRequiresProvisionScope(t *testing.T) {
	runOnly := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"run"}}}
	fake := &fakeProvisioning{create: ProvisionResult{Body: []byte(`{"id":"x_1"}`)}, read: ProvisionResult{Body: []byte(`{}`)}}
	base := provisioningTestServer(t, runOnly, fake)

	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/organizations"}, {"GET", "/v1/organizations"},
		{"POST", "/v1/projects"}, {"GET", "/v1/projects"}, {"PATCH", "/v1/projects/prj_1"},
		{"POST", "/v1/api-keys"}, {"GET", "/v1/api-keys"}, {"POST", "/v1/api-keys/key_1/revoke"},
	} {
		if resp := do(t, route.method, base+route.path, `{}`, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with run-only key status = %d, want 403", route.method, route.path, resp.StatusCode)
		}
	}
	if fake.lastMethod != "" {
		t.Fatalf("the store was reached (%s) despite an insufficient scope — the gate leaked", fake.lastMethod)
	}
}

// TestAPIKeyPlaintextOnlyInCreateResponse is the secret-hygiene contract: the plaintext `key` field appears
// in the create body (the ONE place a key is disclosed) and never in a read, because the handler writes the
// store's Body verbatim and the store's read projections omit it.
func TestAPIKeyPlaintextOnlyInCreateResponse(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeProvisioning{
		create: ProvisionResult{Body: []byte(`{"id":"key_1","object":"api_key","key":"sk_secret","scopes":[]}`)},
		read:   ProvisionResult{Body: []byte(`{"id":"key_1","object":"api_key","scopes":[]}`)},
	}
	base := provisioningTestServer(t, admin, fake)

	created := readBody(t, do(t, "POST", base+"/v1/api-keys", `{"project_id":"prj_1"}`, nil))
	if !strings.Contains(created, `"key":"sk_secret"`) {
		t.Fatalf("create response %q does not carry the plaintext key", created)
	}
	got := readBody(t, do(t, "GET", base+"/v1/api-keys/key_1", ``, nil))
	if strings.Contains(got, "sk_secret") || strings.Contains(got, `"key"`) {
		t.Fatalf("read response %q disclosed a plaintext key", got)
	}
}

// TestProvisioningRoutesUnmountedWhenNil proves the nil-seam guard: a tier that wires no provisioning API
// mounts no provisioning route (a POST is 404), so the Docker-free conformance tiers stay unaffected.
func TestProvisioningRoutesUnmountedWhenNil(t *testing.T) {
	base := provisioningTestServer(t, scopedVerifier{middleware.Scope{Organization: "org_1"}}, nil)
	if resp := do(t, "POST", base+"/v1/organizations", `{}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil provisioning POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var buf strings.Builder
	dec := json.NewDecoder(resp.Body)
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	buf.Write(raw)
	return buf.String()
}
