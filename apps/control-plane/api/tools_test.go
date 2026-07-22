package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeToolRegistry scripts each seam outcome so the handler contract is exercised without a database.
type fakeToolRegistry struct {
	createTool ToolResult
	createRev  ToolResult
	publishRev ToolResult
	createSet  ToolResult
	publishSet ToolResult
	lastBody   []byte
}

func (f *fakeToolRegistry) CreateTool(_ context.Context, _ middleware.Scope, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createTool, nil
}
func (f *fakeToolRegistry) CreateToolRevision(_ context.Context, _ middleware.Scope, _ string, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createRev, nil
}
func (f *fakeToolRegistry) PublishToolRevision(context.Context, middleware.Scope, string) (ToolResult, error) {
	return f.publishRev, nil
}
func (f *fakeToolRegistry) CreateToolSetRevision(_ context.Context, _ middleware.Scope, _ string, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createSet, nil
}
func (f *fakeToolRegistry) PublishToolSetRevision(context.Context, middleware.Scope, string) (ToolResult, error) {
	return f.publishSet, nil
}

func toolTestServer(t *testing.T, reg *fakeToolRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, reg, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestToolManagementSurface pins the /v1/tools + /v1/tool-sets routes (spec §20.2, §28.2-28.4): a valid
// create is a 201 with a Location; an unsupported field is a 400; a name collision is a 409; a publish is
// idempotent (200); and an unknown tool/revision is a 404.
func TestToolManagementSurface(t *testing.T) {
	reg := &fakeToolRegistry{
		createTool: ToolResult{Body: []byte(`{"id":"tool_1","object":"tool"}`)},
		createRev:  ToolResult{Body: []byte(`{"id":"trev_1","object":"tool_revision"}`)},
		publishRev: ToolResult{Body: []byte(`{"id":"trev_1","status":"published"}`)},
		createSet:  ToolResult{Body: []byte(`{"id":"tsrev_1","object":"tool_set_revision"}`)},
		publishSet: ToolResult{Body: []byte(`{"id":"tsrev_1","status":"published"}`)},
	}
	base := toolTestServer(t, reg)

	// Create a tool: 201 with a Location pointing at the minted id.
	resp := do(t, "POST", base+"/v1/tools", `{"canonical_name":"acme.search.fetch"}`, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create tool status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/tools/tool_1" {
		t.Fatalf("create tool Location = %q, want /v1/tools/tool_1", loc)
	}

	// Create a revision: 201.
	if resp := do(t, "POST", base+"/v1/tools/tool_1/revisions", `{"executor":"control_plane","input_schema":{"type":"object"}}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create revision status = %d, want 201", resp.StatusCode)
	}

	// Publish a revision: 200 (idempotent).
	if resp := do(t, "POST", base+"/v1/tools/tool_1/revisions/trev_1/publish", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("publish revision status = %d, want 200", resp.StatusCode)
	}

	// Create + publish a set revision.
	if resp := do(t, "POST", base+"/v1/tool-sets/reviewers/revisions", `{"tools":[{"tool_revision_id":"trev_1"}]}`, nil); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create set revision status = %d, want 201", resp.StatusCode)
	}
	if resp := do(t, "POST", base+"/v1/tool-sets/reviewers/revisions/tsrev_1/publish", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("publish set revision status = %d, want 200", resp.StatusCode)
	}

	// An unsupported field is a 400.
	reg.createRev = ToolResult{BadField: true}
	if resp := do(t, "POST", base+"/v1/tools/tool_1/revisions", `{"executor":"x","credential":"sk"}`, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unsupported-field status = %d, want 400", resp.StatusCode)
	}

	// A name collision is a 409.
	reg.createTool = ToolResult{Conflict: true}
	if resp := do(t, "POST", base+"/v1/tools", `{"canonical_name":"acme.search.fetch"}`, nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("collision status = %d, want 409", resp.StatusCode)
	}

	// An unknown tool is a 404.
	reg.createRev = ToolResult{NotFound: true}
	if resp := do(t, "POST", base+"/v1/tools/tool_missing/revisions", `{"executor":"control_plane","input_schema":{}}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-tool status = %d, want 404", resp.StatusCode)
	}
}

// TestToolRoutesUnmountedWhenNil proves the nil-seam guard: a tier that passes no tool registry never
// mounts the routes, so a POST is a 404 (the agents/webhooks/schedules nil-guard precedent).
func TestToolRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/tools", `{"canonical_name":"acme.search.fetch"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil tool registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
