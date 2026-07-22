package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeSecretRefs scripts each seam outcome so the secret-ref handler contract is exercised without a
// database. One result backs the write ops and another the reads; the recorded scope/body let a test
// assert the provision gate ran and that the write-only value reached the store (never a response).
type fakeSecretRefs struct {
	write      ProvisionResult
	read       ProvisionResult
	lastScope  middleware.Scope
	lastBody   []byte
	lastName   string
	lastMethod string
}

func (f *fakeSecretRefs) CreateSecretRef(_ context.Context, s middleware.Scope, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastBody, f.lastMethod = s, b, "CreateSecretRef"
	return f.write, nil
}
func (f *fakeSecretRefs) ListSecretRefs(_ context.Context, s middleware.Scope) (ProvisionResult, error) {
	f.lastScope, f.lastMethod = s, "ListSecretRefs"
	return f.read, nil
}
func (f *fakeSecretRefs) GetSecretRef(_ context.Context, s middleware.Scope, name string) (ProvisionResult, error) {
	f.lastScope, f.lastName = s, name
	return f.read, nil
}
func (f *fakeSecretRefs) RotateSecretRef(_ context.Context, s middleware.Scope, name string, b []byte) (ProvisionResult, error) {
	f.lastScope, f.lastName, f.lastBody, f.lastMethod = s, name, b, "RotateSecretRef"
	return f.write, nil
}

func secretRefTestServer(t *testing.T, verifier middleware.Verifier, api SecretRefAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, WithSecretRefs(api)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestSecretRefSurface pins the routing + rendering contract: a create is 201, a read/list/rotate is 200,
// a strict-decode reject is a 400, and an absent name is a 404. An admin key (empty scopes) passes the gate.
func TestSecretRefSurface(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeSecretRefs{
		write: ProvisionResult{Body: []byte(`{"name":"provider-one","version":1,"object":"secret_ref"}`)},
		read:  ProvisionResult{Body: []byte(`{"name":"provider-one","version":1,"object":"secret_ref"}`)},
	}
	base := secretRefTestServer(t, admin, fake)

	cases := []struct {
		method, path, body string
		wantStatus         int
	}{
		{"POST", "/v1/secret-refs", `{"name":"provider-one","value":"sk-upstream-abc"}`, http.StatusCreated},
		{"GET", "/v1/secret-refs", ``, http.StatusOK},
		{"GET", "/v1/secret-refs/provider-one", ``, http.StatusOK},
		{"POST", "/v1/secret-refs/provider-one/rotate", `{"value":"sk-upstream-def"}`, http.StatusOK},
	}
	for _, c := range cases {
		resp := do(t, c.method, base+c.path, c.body, nil)
		if resp.StatusCode != c.wantStatus {
			t.Fatalf("%s %s status = %d, want %d", c.method, c.path, resp.StatusCode, c.wantStatus)
		}
		resp.Body.Close()
	}

	fake.write = ProvisionResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/secret-refs", `{"nope":1}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown-field create status = %d, want 400", resp.StatusCode)
	}
	fake.write = ProvisionResult{MissingField: "value"}
	if resp := do(t, "POST", base+"/v1/secret-refs", `{"name":"x"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing-field create status = %d, want 400", resp.StatusCode)
	}
	fake.read = ProvisionResult{NotFound: true}
	if resp := do(t, "GET", base+"/v1/secret-refs/missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown secret status = %d, want 404", resp.StatusCode)
	}
}

// TestSecretRefValueNeverEchoed is the write-only contract: the value goes IN on create/rotate but the
// handler writes only the store's metadata Body, so no response ever carries the plaintext.
func TestSecretRefValueNeverEchoed(t *testing.T) {
	admin := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1"}}
	fake := &fakeSecretRefs{
		write: ProvisionResult{Body: []byte(`{"name":"provider-one","version":2,"object":"secret_ref"}`)},
		read:  ProvisionResult{Body: []byte(`{"name":"provider-one","version":2,"object":"secret_ref"}`)},
	}
	base := secretRefTestServer(t, admin, fake)

	created := readBody(t, do(t, "POST", base+"/v1/secret-refs", `{"name":"provider-one","value":"sk-secret-xyz"}`, nil))
	if strings.Contains(created, "sk-secret-xyz") || strings.Contains(created, `"value"`) {
		t.Fatalf("create response %q disclosed the secret value", created)
	}
	if !strings.Contains(string(fake.lastBody), "sk-secret-xyz") {
		t.Fatalf("the value did not reach the store; lastBody = %q", fake.lastBody)
	}
	rotated := readBody(t, do(t, "POST", base+"/v1/secret-refs/provider-one/rotate", `{"value":"sk-rotated-2"}`, nil))
	if strings.Contains(rotated, "sk-rotated-2") || strings.Contains(rotated, `"value"`) {
		t.Fatalf("rotate response %q disclosed the secret value", rotated)
	}
}

// TestSecretRefRequiresProvisionScope proves the basic-scope gate: a key whose non-empty scopes omit
// `provision` is refused (403) on every secret-ref route and the store is never reached.
func TestSecretRefRequiresProvisionScope(t *testing.T) {
	runOnly := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Scopes: []string{"run"}}}
	fake := &fakeSecretRefs{write: ProvisionResult{Body: []byte(`{"name":"x"}`)}, read: ProvisionResult{Body: []byte(`{}`)}}
	base := secretRefTestServer(t, runOnly, fake)

	for _, route := range []struct{ method, path string }{
		{"POST", "/v1/secret-refs"}, {"GET", "/v1/secret-refs"},
		{"GET", "/v1/secret-refs/provider-one"}, {"POST", "/v1/secret-refs/provider-one/rotate"},
	} {
		if resp := do(t, route.method, base+route.path, `{}`, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("%s %s with run-only key status = %d, want 403", route.method, route.path, resp.StatusCode)
		}
	}
	if fake.lastMethod != "" {
		t.Fatalf("the store was reached (%s) despite an insufficient scope — the gate leaked", fake.lastMethod)
	}
}

// TestSecretRefRoutesUnmountedWhenNil proves the nil-seam guard: a tier that wires no secret-ref API mounts
// no secret-ref route (a POST is 404), so the Docker-free conformance tiers stay unaffected.
func TestSecretRefRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(scopedVerifier{middleware.Scope{Organization: "org_1"}}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/secret-refs", `{}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil secret-ref POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
