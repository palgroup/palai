package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeAgentRegistry scripts the read side of AgentRegistry; the write methods are unused no-ops here.
type fakeAgentRegistry struct {
	profile   AgentResult
	profiles  []ListRow
	revisions []ListRow
}

func (f *fakeAgentRegistry) CreateAgentProfile(context.Context, middleware.Scope, string) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) CreateAgentRevision(context.Context, middleware.Scope, string, []byte) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) PublishAgentRevision(context.Context, middleware.Scope, string) (AgentResult, error) {
	return AgentResult{}, nil
}
func (f *fakeAgentRegistry) CreateRunTemplateRevision(context.Context, middleware.Scope, string, []byte) (AgentResult, error) {
	return AgentResult{}, nil
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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
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
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, reg, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
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
