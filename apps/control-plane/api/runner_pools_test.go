package api

// The E24 T2 pool read surface, at the router. What is asserted here is the WIRING and the RENDERING —
// which tenant the handler asks for, and what the caller sees — because the tenancy itself is RLS's
// and is proved against a real Postgres in internal/fleet's component leg. A router test that faked a
// database and then asserted the fake was scoped would be a test of the fake.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeRunnerRegistry records the tenant the handler asked for and answers with one pool.
type fakeRunnerRegistry struct {
	askedOrg     string
	askedProject string
	pools        []RunnerPoolItem
	// keyFound scripts the store's found=false leg, which is how a pool (or key) belonging to another
	// tenant is rendered as a non-disclosing 404.
	keyFound      bool
	mintedForPool string
	mintedExpiry  *time.Time
}

func (f *fakeRunnerRegistry) ListRunners(context.Context, string, string, RunnerListWindow) ([]RunnerItem, error) {
	return nil, nil
}

func (f *fakeRunnerRegistry) GetRunner(context.Context, string, string, string) (RunnerItem, bool, error) {
	return RunnerItem{}, false, nil
}

func (f *fakeRunnerRegistry) ListRunnerPools(_ context.Context, org, project string, _ RunnerListWindow) ([]RunnerPoolItem, error) {
	f.askedOrg, f.askedProject = org, project
	return f.pools, nil
}

func runnerPoolsRouter(t *testing.T, registry RunnerRegistryAPI) *httptest.Server {
	t.Helper()
	opts := []RouterOption{}
	if registry != nil {
		opts = append(opts, WithRunners(registry))
	}
	ts := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, opts...))
	t.Cleanup(ts.Close)
	return ts
}

func getRunnerPools(t *testing.T, ts *httptest.Server) (int, []byte) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, ts.URL+"/v1/runner-pools", nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("GET /v1/runner-pools: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, body
}

// TestRunnerPoolsRenderThePostureForTheCallersTenant pins the two things this route exists for: it asks
// the store for the VERIFIED bearer's tenant and nothing the caller supplied, and it renders the
// posture — which is the whole reason an operator reads this list, since a runner's pool_id is an
// opaque id until something says what that pool IS.
func TestRunnerPoolsRenderThePostureForTheCallersTenant(t *testing.T) {
	registry := &fakeRunnerRegistry{pools: []RunnerPoolItem{{
		ID: "pool_mac", Name: "mac-pool", Posture: "unsandboxed-host", OS: "darwin", Arch: "arm64",
		StrictEnrollment: true, CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}}}
	status, body := getRunnerPools(t, runnerPoolsRouter(t, registry))
	if status != http.StatusOK {
		t.Fatalf("GET /v1/runner-pools = %d, want 200: %s", status, body)
	}
	if registry.askedOrg != "org_1" || registry.askedProject != "prj_1" {
		t.Fatalf("the handler asked for tenant (%q,%q), want the verified bearer's (org_1,prj_1)",
			registry.askedOrg, registry.askedProject)
	}
	var page struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("decode page: %v (%s)", err, body)
	}
	if len(page.Data) != 1 {
		t.Fatalf("page carries %d row(s), want 1: %s", len(page.Data), body)
	}
	row := page.Data[0]
	for field, want := range map[string]any{
		"id": "pool_mac", "object": "runner_pool", "name": "mac-pool",
		"posture": "unsandboxed-host", "os": "darwin", "arch": "arm64", "strict_enrollment": true,
	} {
		if row[field] != want {
			t.Fatalf("rendered pool %s = %v, want %v (%s)", field, row[field], want, body)
		}
	}
}

// TestRunnerPoolsRouteUnmountedWithoutARegistry is the other half of the router's optional-surface
// rule: a binary that wires no registry must 404 the route rather than 500 on a nil seam.
func TestRunnerPoolsRouteUnmountedWithoutARegistry(t *testing.T) {
	status, body := getRunnerPools(t, runnerPoolsRouter(t, nil))
	if status != http.StatusNotFound {
		t.Fatalf("unmounted GET /v1/runner-pools = %d, want 404: %s", status, body)
	}
}

// THE KEY SURFACE AT THE ROUTER (E24 T3). What is asserted here is the RENDERING and the GATE — that
// the value appears on exactly one response and that a key without the `provision` capability cannot
// mint one. The credential behaviour itself (scope, revocation, the digest) is proved against a real
// Postgres in internal/execution's component leg; a router test that faked a store and then asserted
// the fake had hashed something would be a test of the fake.

func (f *fakeRunnerRegistry) MintRunnerPoolKey(_ context.Context, org, project, poolID string, expiresAt *time.Time) (RunnerPoolKeyItem, bool, error) {
	f.askedOrg, f.askedProject = org, project
	if !f.keyFound {
		return RunnerPoolKeyItem{}, false, nil
	}
	f.mintedForPool, f.mintedExpiry = poolID, expiresAt
	return RunnerPoolKeyItem{
		ID: "rpk_1", PoolID: poolID, Prefix: "rpk_abcd", Value: "rpk_abcdthisisthevalue",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(), ExpiresAt: expiresAt,
	}, true, nil
}

func (f *fakeRunnerRegistry) ListRunnerPoolKeys(_ context.Context, org, project, poolID string) ([]RunnerPoolKeyItem, error) {
	f.askedOrg, f.askedProject = org, project
	// The listed row carries a Value the RENDERER must drop: if the projection ever leaked one, this is
	// the value that would show up in the assertion below.
	return []RunnerPoolKeyItem{{
		ID: "rpk_1", PoolID: poolID, Prefix: "rpk_abcd", Value: "rpk_abcdthisisthevalue",
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}}, nil
}

func (f *fakeRunnerRegistry) RevokeRunnerPoolKey(_ context.Context, org, project, keyID string) (RunnerPoolKeyItem, bool, error) {
	f.askedOrg, f.askedProject = org, project
	if !f.keyFound {
		return RunnerPoolKeyItem{}, false, nil
	}
	revoked := time.Unix(1_700_000_100, 0).UTC()
	return RunnerPoolKeyItem{
		ID: keyID, PoolID: "pool_mac", Prefix: "rpk_abcd", CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
		RevokedAt: &revoked,
		EnrolledRunners: []RunnerPoolKeyEnrollment{{
			ID: "rnr_1", Label: "runner-local", DNS: "rnr_1.runners.palai.internal", State: "active",
			PoolID: "pool_mac", EnrolledAt: time.Unix(1_700_000_050, 0).UTC(),
		}},
	}, true, nil
}

// scopedRouter serves the router with a verifier whose key carries `scopes`. An empty slice is the
// unrestricted admin key (Scope.HasScope's documented rule); a non-empty one without `provision` is what
// the gate must refuse.
func scopedRouter(t *testing.T, registry RunnerRegistryAPI, scopes []string) *httptest.Server {
	t.Helper()
	// scopedVerifier is model_routes_test.go's — reused rather than re-declared, which is the whole point
	// of a package-level test helper.
	verifier := scopedVerifier{middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1", Scopes: scopes}}
	ts := httptest.NewServer(NewRouter(verifier, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil, WithRunners(registry)))
	t.Cleanup(ts.Close)
	return ts
}

func doKeyRequest(t *testing.T, ts *httptest.Server, method, path string, payload string) (int, []byte) {
	t.Helper()
	var body io.Reader
	if payload != "" {
		body = strings.NewReader(payload)
	}
	request, err := http.NewRequest(method, ts.URL+path, body)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-key")
	resp, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// TestRunnerPoolKeyValueIsRenderedOnCreateAndNowhereElse is the rendering claim, and it is asserted on
// BOTH sides so it cannot pass by rendering nothing: create carries `key`, and the listing of the SAME
// row — whose store answer deliberately still holds a Value — does not.
func TestRunnerPoolKeyValueIsRenderedOnCreateAndNowhereElse(t *testing.T) {
	registry := &fakeRunnerRegistry{keyFound: true}
	ts := scopedRouter(t, registry, []string{"provision"})

	status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runner-pools/pool_mac/keys", `{"expires_at":"2030-01-01T00:00:00Z"}`)
	if status != http.StatusCreated {
		t.Fatalf("POST keys = %d, want 201: %s", status, body)
	}
	var created map[string]any
	if err := json.Unmarshal(body, &created); err != nil {
		t.Fatalf("decode create: %v (%s)", err, body)
	}
	if created["key"] != "rpk_abcdthisisthevalue" {
		t.Fatalf("create response carries key = %v, want the one-time value (%s)", created["key"], body)
	}
	if created["key_prefix"] != "rpk_abcd" || created["pool_id"] != "pool_mac" {
		t.Fatalf("create response = %s, want the prefix and the pool it admits into", body)
	}
	if registry.mintedForPool != "pool_mac" {
		t.Fatalf("the handler minted for pool %q, want the path's pool_mac", registry.mintedForPool)
	}
	if registry.mintedExpiry == nil || registry.mintedExpiry.Year() != 2030 {
		t.Fatalf("the requested expiry did not reach the store: %v", registry.mintedExpiry)
	}
	if registry.askedOrg != "org_1" || registry.askedProject != "prj_1" {
		t.Fatalf("the handler asked for tenant (%q,%q), want the verified bearer's", registry.askedOrg, registry.askedProject)
	}

	status, body = doKeyRequest(t, ts, http.MethodGet, "/v1/runner-pools/pool_mac/keys", "")
	if status != http.StatusOK {
		t.Fatalf("GET keys = %d, want 200: %s", status, body)
	}
	if strings.Contains(string(body), "rpk_abcdthisisthevalue") {
		t.Fatalf("the LISTING carries a key value: %s", body)
	}
	if !strings.Contains(string(body), "rpk_abcd") {
		t.Fatalf("the listing carries no prefix, so an operator cannot tell two keys apart: %s", body)
	}
}

// TestRunnerPoolKeyRevokeNamesTheMachinesItDidNotStop is the operator-facing half of revocation: the
// answer names the machines the key already admitted, under a field name that says they keep running.
func TestRunnerPoolKeyRevokeNamesTheMachinesItDidNotStop(t *testing.T) {
	ts := scopedRouter(t, &fakeRunnerRegistry{keyFound: true}, []string{"provision"})
	status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runner-pool-keys/rpk_1/revoke", "")
	if status != http.StatusOK {
		t.Fatalf("POST revoke = %d, want 200: %s", status, body)
	}
	var revoked map[string]any
	if err := json.Unmarshal(body, &revoked); err != nil {
		t.Fatalf("decode revoke: %v (%s)", err, body)
	}
	machines, ok := revoked["enrolled_runners_still_running"].([]any)
	if !ok || len(machines) != 1 {
		t.Fatalf("revoke response does not name the machines it did not stop: %s", body)
	}
	if revoked["revoked_at"] == nil {
		t.Fatalf("revoke response carries no revoked_at: %s", body)
	}
	if _, leaked := revoked["key"]; leaked {
		t.Fatalf("a revoke response carries a key value: %s", body)
	}
}

// TestRunnerPoolKeyRoutesRequireTheProvisionCapability is the gate. A key with SOME scopes but not
// `provision` is refused on all three routes; the unrestricted admin key (empty scopes) is not, which is
// what makes this a test of the gate rather than of a blanket refusal.
func TestRunnerPoolKeyRoutesRequireTheProvisionCapability(t *testing.T) {
	limited := scopedRouter(t, &fakeRunnerRegistry{keyFound: true}, []string{"run"})
	for _, call := range []struct{ method, path string }{
		{http.MethodPost, "/v1/runner-pools/pool_mac/keys"},
		{http.MethodGet, "/v1/runner-pools/pool_mac/keys"},
		{http.MethodPost, "/v1/runner-pool-keys/rpk_1/revoke"},
	} {
		status, body := doKeyRequest(t, limited, call.method, call.path, "")
		if status != http.StatusForbidden {
			t.Fatalf("%s %s with a non-provision key = %d, want 403: %s", call.method, call.path, status, body)
		}
	}
	admin := scopedRouter(t, &fakeRunnerRegistry{keyFound: true}, nil)
	if status, body := doKeyRequest(t, admin, http.MethodGet, "/v1/runner-pools/pool_mac/keys", ""); status != http.StatusOK {
		t.Fatalf("GET keys with an unrestricted admin key = %d, want 200: %s", status, body)
	}
}

// TestRunnerPoolKeyMintOnAForeignPoolIs404 pins the non-disclosure: a pool that is not this tenant's is
// indistinguishable from one that does not exist.
func TestRunnerPoolKeyMintOnAForeignPoolIs404(t *testing.T) {
	ts := scopedRouter(t, &fakeRunnerRegistry{keyFound: false}, []string{"provision"})
	if status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runner-pools/pool_someone_else/keys", ""); status != http.StatusNotFound {
		t.Fatalf("POST keys on a foreign pool = %d, want 404: %s", status, body)
	}
	if status, body := doKeyRequest(t, ts, http.MethodPost, "/v1/runner-pool-keys/rpk_other/revoke", ""); status != http.StatusNotFound {
		t.Fatalf("POST revoke on a foreign key = %d, want 404: %s", status, body)
	}
}
