package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeMCPRegistry scripts each seam outcome so the handler contract is exercised without a database.
type fakeMCPRegistry struct {
	create   MCPConnectionResult
	discover MCPConnectionResult
	lastBody []byte
	lastID   string
}

func (f *fakeMCPRegistry) CreateMCPConnection(_ context.Context, _ middleware.Scope, body []byte) (MCPConnectionResult, error) {
	f.lastBody = body
	return f.create, nil
}
func (f *fakeMCPRegistry) DiscoverMCPConnection(_ context.Context, _ middleware.Scope, id string) (MCPConnectionResult, error) {
	f.lastID = id
	return f.discover, nil
}

func mcpTestServer(t *testing.T, reg *fakeMCPRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestMCPConnectionManagementSurface pins the ADMIN routes (spec §28.13): a valid create is a 201 with a
// Location; the discover action is a 200; an invalid config is a 400; a name collision is a 409; an unknown
// connection discover is a 404. There is deliberately no model-facing surface here — these are admin routes.
func TestMCPConnectionManagementSurface(t *testing.T) {
	reg := &fakeMCPRegistry{
		create:   MCPConnectionResult{Body: []byte(`{"id":"mcpc_1","object":"mcp_connection"}`)},
		discover: MCPConnectionResult{Body: []byte(`{"object":"mcp_discovery","new_revisions":["echo"]}`)},
	}
	base := mcpTestServer(t, reg)

	resp := do(t, "POST", base+"/v1/mcp-connections", `{"name":"docs","transport":"stdio","config":{"image_digest":"sha256:x","cmd":["/mcp"]}}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create connection status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/mcp-connections/mcpc_1" {
		t.Fatalf("create Location = %q, want /v1/mcp-connections/mcpc_1", loc)
	}

	if resp := do(t, "POST", base+"/v1/mcp-connections/mcpc_1/discover", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("discover status = %d, want 200", resp.StatusCode)
	}
	if reg.lastID != "mcpc_1" {
		t.Fatalf("discover id = %q, want mcpc_1", reg.lastID)
	}

	reg.create = MCPConnectionResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/mcp-connections", `{"name":"x","transport":"bad","config":{}}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid-config status = %d, want 400", resp.StatusCode)
	}
	reg.create = MCPConnectionResult{Conflict: true}
	if resp := do(t, "POST", base+"/v1/mcp-connections", `{"name":"docs","transport":"stdio","config":{}}`, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("collision status = %d, want 409", resp.StatusCode)
	}
	reg.discover = MCPConnectionResult{NotFound: true}
	if resp := do(t, "POST", base+"/v1/mcp-connections/mcpc_missing/discover", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-connection discover status = %d, want 404", resp.StatusCode)
	}
}

// TestMCPRoutesUnmountedWhenNil proves the nil-seam guard AND the model-facing-absence posture: a tier that
// passes no MCP registry mounts no MCP route at all (a POST is 404). MCP add/discover is an admin API
// surface only — there is no model-callable tool for it (the broker exposes no such name).
func TestMCPRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/mcp-connections", `{"name":"docs"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil MCP registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
