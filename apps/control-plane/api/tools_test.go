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
	getTool    ToolResult
	listTools  []ListRow
	listSets   []ListRow
	listRevs   []ListRow
	// revsFound scripts whether the lineage exists in scope — the bool that separates a 404 from an empty
	// page on GET /v1/tools/{tool_id}/revisions (E25 T7).
	revsFound bool
	getSetRev ToolResult
	// lastRevisionsTool records which lineage the handler asked for, so the path segment is proven to
	// reach the seam rather than assumed from a 200.
	lastRevisionsTool string
	lastSetRevision   [2]string
	lastBody          []byte
}

func (f *fakeToolRegistry) CreateTool(_ context.Context, _ middleware.Scope, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createTool, nil
}
func (f *fakeToolRegistry) CreateToolRevision(_ context.Context, _ middleware.Scope, _ string, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createRev, nil
}
func (f *fakeToolRegistry) PublishToolRevision(context.Context, middleware.Scope, string, []byte) (ToolResult, error) {
	return f.publishRev, nil
}
func (f *fakeToolRegistry) CreateToolSetRevision(_ context.Context, _ middleware.Scope, _ string, body []byte) (ToolResult, error) {
	f.lastBody = body
	return f.createSet, nil
}
func (f *fakeToolRegistry) PublishToolSetRevision(context.Context, middleware.Scope, string) (ToolResult, error) {
	return f.publishSet, nil
}
func (f *fakeToolRegistry) GetTool(_ context.Context, _ middleware.Scope, _ string) (ToolResult, error) {
	return f.getTool, nil
}
func (f *fakeToolRegistry) ListTools(_ context.Context, _ middleware.Scope, _ ListQuery) ([]ListRow, error) {
	return f.listTools, nil
}
func (f *fakeToolRegistry) ListToolSets(_ context.Context, _ middleware.Scope, _ ListQuery) ([]ListRow, error) {
	return f.listSets, nil
}

func (f *fakeToolRegistry) ListToolRevisions(_ context.Context, _ middleware.Scope, toolID string, _ ListQuery) ([]ListRow, bool, error) {
	f.lastRevisionsTool = toolID
	return f.listRevs, f.revsFound, nil
}
func (f *fakeToolRegistry) GetToolSetRevision(_ context.Context, _ middleware.Scope, setName, revisionID string) (ToolResult, error) {
	f.lastSetRevision = [2]string{setName, revisionID}
	return f.getSetRev, nil
}

func toolTestServer(t *testing.T, reg *fakeToolRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
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

// TestToolReadRoutes pins the E13 T4 read side: GET a tool lineage, LIST tools, and LIST tool-sets each
// render over the shared Page envelope; a foreign tool id is a 404.
func TestToolReadRoutes(t *testing.T) {
	reg := &fakeToolRegistry{
		getTool:   ToolResult{Body: []byte(`{"id":"tool_1","object":"tool"}`)},
		listTools: []ListRow{{ID: "tool_1", Body: []byte(`{"id":"tool_1"}`)}},
		listSets:  []ListRow{{ID: "tsrev_1", Body: []byte(`{"id":"tsrev_1"}`)}, {ID: "tsrev_2", Body: []byte(`{"id":"tsrev_2"}`)}},
	}
	base := toolTestServer(t, reg)

	if resp := do(t, "GET", base+"/v1/tools/tool_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("get tool status = %d, want 200", resp.StatusCode)
	}
	assertPageLen(t, do(t, "GET", base+"/v1/tools", ``, nil), 1)
	assertPageLen(t, do(t, "GET", base+"/v1/tool-sets", ``, nil), 2)

	reg.getTool = ToolResult{NotFound: true}
	if resp := do(t, "GET", base+"/v1/tools/tool_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown tool get status = %d, want 404", resp.StatusCode)
	}
}

// narrowedVerifier authenticates a key whose scope set is NON-EMPTY and does not name `provision`. It is
// the only shape the capability gate can be measured with: an empty scope set is unrestricted by design
// (middleware.Scope.HasScope), so a bootstrap key would pass the gate and prove nothing about it.
type narrowedVerifier struct{}

func (narrowedVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return middleware.Scope{Organization: "org_1", Project: "prj_1", Principal: "prin_1", Scopes: []string{"responses"}}, nil
}

// TestToolRevisionReadRoutes pins the two E25 T7 reads at the HANDLER contract: the path segments reach the
// seam, an unknown lineage is a 404 rather than an empty page, and a set revision renders through the
// single-resource path.
func TestToolRevisionReadRoutes(t *testing.T) {
	reg := &fakeToolRegistry{
		revsFound: true,
		listRevs: []ListRow{
			{ID: "trev_2", Body: []byte(`{"id":"trev_2","status":"draft"}`)},
			{ID: "trev_1", Body: []byte(`{"id":"trev_1","status":"published"}`)},
		},
		getSetRev: ToolResult{Body: []byte(`{"id":"tsrev_1","tools":[{"tool_revision_id":"trev_1"}]}`)},
	}
	base := toolTestServer(t, reg)

	assertPageLen(t, do(t, "GET", base+"/v1/tools/tool_1/revisions", ``, nil), 2)
	if reg.lastRevisionsTool != "tool_1" {
		t.Fatalf("the handler asked the seam for %q, want the tool_id from the path", reg.lastRevisionsTool)
	}

	// AN UNKNOWN LINEAGE IS A 404, NOT AN EMPTY PAGE. An empty page would tell an operator following the
	// runbook that their newly discovered tool has no revisions, when what happened is a mistyped id.
	reg.revsFound = false
	reg.listRevs = nil
	if resp := do(t, "GET", base+"/v1/tools/tool_missing/revisions", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown-lineage revisions status = %d, want 404", resp.StatusCode)
	}

	if resp := do(t, "GET", base+"/v1/tool-sets/jira/revisions/tsrev_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("get set revision status = %d, want 200", resp.StatusCode)
	}
	if reg.lastSetRevision != [2]string{"jira", "tsrev_1"} {
		t.Fatalf("the handler asked the seam for %v, want both path segments", reg.lastSetRevision)
	}
	reg.getSetRev = ToolResult{NotFound: true}
	if resp := do(t, "GET", base+"/v1/tool-sets/jira/revisions/tsrev_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown set revision status = %d, want 404", resp.StatusCode)
	}
}

// TestToolRevisionReadRoutesRequireProvision pins the capability gate on the two NEW routes and, in the
// same test, pins that the E12/E13 routes stay UNGATED — because the asymmetry is a decision and a test
// that only asserted the refusals would let a later "tidy-up" gate the whole family silently.
func TestToolRevisionReadRoutesRequireProvision(t *testing.T) {
	reg := &fakeToolRegistry{revsFound: true, getTool: ToolResult{Body: []byte(`{"id":"tool_1"}`)}}
	srv := httptest.NewServer(NewRouter(narrowedVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/v1/tools/tool_1/revisions", "/v1/tool-sets/jira/revisions/tsrev_1"} {
		if resp := do(t, "GET", srv.URL+path, ``, nil); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("GET %s with a key lacking `provision` = %d, want 403", path, resp.StatusCode)
		}
	}
	if resp := do(t, "GET", srv.URL+"/v1/tools/tool_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/tools/{id} with the same key = %d, want 200 — this task does not retro-gate a shipped route", resp.StatusCode)
	}
}

// TestToolRoutesUnmountedWhenNil proves the nil-seam guard: a tier that passes no tool registry never
// mounts the routes, so a POST is a 404 (the agents/webhooks/schedules nil-guard precedent).
func TestToolRoutesUnmountedWhenNil(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)
	if resp := do(t, "POST", srv.URL+"/v1/tools", `{"canonical_name":"acme.search.fetch"}`, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("nil tool registry POST status = %d, want 404 (route unmounted)", resp.StatusCode)
	}
}
