package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeAgentRegistry scripts the read side of AgentRegistry; the write methods are unused no-ops here,
// except the two revision creates, whose 201 shape TestAgentRevisionCreatesCarryTheIdWithoutAnAddress pins.
type fakeAgentRegistry struct {
	profile     AgentResult
	profiles    []ListRow
	revisions   []ListRow
	agentRev    AgentResult
	templateRev AgentResult
}

func (f *fakeAgentRegistry) CreateAgentProfile(context.Context, middleware.Scope, string) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) CreateAgentRevision(context.Context, middleware.Scope, string, []byte) (AgentResult, error) {
	return f.agentRev, nil
}
func (f *fakeAgentRegistry) PublishAgentRevision(context.Context, middleware.Scope, string) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) CreateRunTemplateRevision(context.Context, middleware.Scope, string, []byte) (AgentResult, error) {
	return f.templateRev, nil
}
func (f *fakeAgentRegistry) PublishRunTemplateRevision(context.Context, middleware.Scope, string) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) GetAgentProfile(context.Context, middleware.Scope, string) (AgentResult, error) {
	return f.profile, nil
}
func (f *fakeAgentRegistry) ListAgentProfiles(context.Context, middleware.Scope, ListQuery) ([]ListRow, error) {
	return f.profiles, nil
}
func (f *fakeAgentRegistry) ListAgentRevisions(context.Context, middleware.Scope, string, ListQuery) ([]ListRow, error) {
	return f.revisions, nil
}

// TestAgentReadRoutes pins the E13 T4 read side: GET a profile, LIST profiles, LIST a profile's revisions
// each render over the shared Page envelope; a foreign profile id is a 404.
func TestAgentReadRoutes(t *testing.T) {
	reg := &fakeAgentRegistry{
		profile:   AgentResult{Body: []byte(`{"id":"aprof_1","object":"agent"}`)},
		profiles:  []ListRow{{ID: "aprof_1", Body: []byte(`{"id":"aprof_1"}`)}},
		revisions: []ListRow{{ID: "arev_1", Body: []byte(`{"id":"arev_1"}`)}, {ID: "arev_2", Body: []byte(`{"id":"arev_2"}`)}},
	}
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	if resp := do(t, "GET", srv.URL+"/v1/agents/aprof_1", ``, nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("get profile status = %d, want 200", resp.StatusCode)
	}
	assertPageLen(t, do(t, "GET", srv.URL+"/v1/agents", ``, nil), 1)
	assertPageLen(t, do(t, "GET", srv.URL+"/v1/agents/aprof_1/revisions", ``, nil), 2)

	reg.profile = AgentResult{NotFound: true}
	if resp := do(t, "GET", srv.URL+"/v1/agents/aprof_missing", ``, nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown profile get status = %d, want 404", resp.StatusCode)
	}
}

// TestAgentRevisionsCursorIsProfileScoped is the review SHOULD 2 guard: a revisions cursor minted on
// profile A must be REJECTED when replayed on profile B's revisions — otherwise it MAC-validates (same
// tenant + flat kind) and silently skips B's rows newer than A's keyset position. Docker-free: the fake
// returns 2 rows so a limit=1 page mints a real next_cursor.
func TestAgentRevisionsCursorIsProfileScoped(t *testing.T) {
	reg := &fakeAgentRegistry{revisions: []ListRow{
		{ID: "arev_1", Body: []byte(`{"id":"arev_1"}`)},
		{ID: "arev_2", Body: []byte(`{"id":"arev_2"}`)},
	}}
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	page := assertPageLen(t, do(t, "GET", srv.URL+"/v1/agents/aprof_A/revisions?limit=1", ``, nil), 1)
	if page.NextCursor == nil {
		t.Fatal("expected a next_cursor to replay across profiles")
	}
	resp := do(t, "GET", srv.URL+"/v1/agents/aprof_B/revisions?limit=1&after="+url.QueryEscape(*page.NextCursor), ``, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("profile-B replay of profile-A's revisions cursor = %d, want 400 (profile-scoped cursor)", resp.StatusCode)
	}
}

// TestAgentRevisionCreatesCarryTheIdWithoutAnAddress is the E29 T2 leg for the two revision creates. Both
// used to name a Location — "/v1/agent-revisions/<id>" and "/v1/run-template-revisions/<id>" — and neither
// prefix has ever been mounted, so following either reached net/http's own not-found.
//
// The two are removed for DIFFERENT reasons, and the difference is worth keeping straight. An agent revision
// is readable: GET /v1/agents/{agent_id}/revisions serves it, so what was wrong was the ADDRESS, not a
// missing capability — and writing the collection's real singular member instead was rejected because that
// member is not mounted either. A run-template revision is readable at NO layer: no route, and the only
// SELECT over run_template_revisions is a published-or-not boolean. Pointing at either would be a caller's
// 404; the honest header is none.
//
// What must NOT change is the id. It is the whole reason the header existed, a caller needs it to publish or
// pin, and after this task the 201 body is where it lives.
func TestAgentRevisionCreatesCarryTheIdWithoutAnAddress(t *testing.T) {
	reg := &fakeAgentRegistry{
		agentRev:    AgentResult{Body: []byte(`{"id":"arev_1","object":"agent_revision"}`)},
		templateRev: AgentResult{Body: []byte(`{"id":"rtrev_1","object":"run_template_revision"}`)},
	}
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	t.Cleanup(srv.Close)

	for _, tc := range []struct{ name, path, wantID string }{
		{"agent revision", "/v1/agents/aprof_1/revisions", "arev_1"},
		{"run-template revision", "/v1/run-templates/tmpl_1/revisions", "rtrev_1"},
	} {
		resp := do(t, "POST", srv.URL+tc.path, `{"model":"m"}`, nil)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("%s create status = %d, want 201", tc.name, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "" {
			t.Fatalf("%s Location = %q, want none: no route serves that prefix, so the header could only be followed into a 404", tc.name, loc)
		}
		if body := readBody(t, resp); !strings.Contains(body, `"`+tc.wantID+`"`) {
			t.Fatalf("%s body = %s, want the minted id %q — dropping the Location must not drop the only way to learn it", tc.name, body, tc.wantID)
		}
	}
}
