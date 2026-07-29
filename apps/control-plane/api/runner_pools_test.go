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
	"testing"
	"time"
)

// fakeRunnerRegistry records the tenant the handler asked for and answers with one pool.
type fakeRunnerRegistry struct {
	askedOrg     string
	askedProject string
	pools        []RunnerPoolItem
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
