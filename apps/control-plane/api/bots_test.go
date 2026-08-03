package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// fakeBotRegistry records what reached the store seam and scripts its outcomes, matching the
// fakeSlackConnectionAPI idiom this file is modelled on.
type fakeBotRegistry struct {
	createdOrg, createdProject string
	created                    BotCreate
	createErr                  error

	items                    []ListRow
	listedOrg, listedProject string
	lastQuery                ListQuery

	missing            bool
	detailBody         []byte
	getOrg, getProject string

	patched                  BotPatch
	patchedID                string
	patchOrg, patchProject   string
	patchErr                 error
	deletedID                string
	deleteOrg, deleteProject string
}

func (f *fakeBotRegistry) CreateBot(_ context.Context, scope middleware.Scope, req BotCreate) (BotResult, error) {
	f.createdOrg, f.createdProject, f.created = scope.Organization, scope.Project, req
	if f.createErr != nil {
		return BotResult{}, f.createErr
	}
	if req.Name == "" || req.Kind == "" {
		return BotResult{Invalid: true}, nil
	}
	config := req.Config
	if len(config) == 0 {
		config = json.RawMessage(`{}`)
	}
	body, _ := json.Marshal(map[string]any{
		"id": "bot_1", "object": "bot", "name": req.Name, "kind": req.Kind,
		"agent_revision_id": req.AgentRevisionID, "repository_binding_id": req.RepositoryBindingID,
		"principal_id": req.PrincipalID, "config": config, "disabled": false,
	})
	return BotResult{Body: body}, nil
}

func (f *fakeBotRegistry) ListBots(_ context.Context, scope middleware.Scope, q ListQuery) ([]ListRow, error) {
	f.listedOrg, f.listedProject, f.lastQuery = scope.Organization, scope.Project, q
	return f.items, nil
}

func (f *fakeBotRegistry) GetBot(_ context.Context, scope middleware.Scope, id string) (BotResult, error) {
	f.getOrg, f.getProject = scope.Organization, scope.Project
	if f.missing {
		return BotResult{NotFound: true}, nil
	}
	body := f.detailBody
	if body == nil {
		body, _ = json.Marshal(map[string]any{
			"id": id, "object": "bot", "name": "ios-bot", "kind": "slack",
			"agent_revision_id": "", "repository_binding_id": "", "principal_id": "",
			"config": json.RawMessage(`{}`), "disabled": false,
		})
	}
	return BotResult{Body: body}, nil
}

func (f *fakeBotRegistry) PatchBot(_ context.Context, scope middleware.Scope, id string, patch BotPatch) (BotResult, error) {
	f.patchOrg, f.patchProject, f.patchedID, f.patched = scope.Organization, scope.Project, id, patch
	if f.patchErr != nil {
		return BotResult{}, f.patchErr
	}
	if f.missing {
		return BotResult{NotFound: true}, nil
	}
	body, _ := json.Marshal(map[string]any{"id": id, "object": "bot"})
	return BotResult{Body: body}, nil
}

func (f *fakeBotRegistry) DeleteBot(_ context.Context, scope middleware.Scope, id string) (bool, error) {
	f.deleteOrg, f.deleteProject, f.deletedID = scope.Organization, scope.Project, id
	return !f.missing, nil
}

func botTestServer(t *testing.T, b BotRegistry) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithBots(b)))
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestBotsAPIStoresConfigOpaquely is the brief's central claim (Task 4): a bot is created with fields the
// control plane has never heard of and reads back byte-for-byte. This is what "the control plane never
// learns what a bot IS" looks like from the wire.
func TestBotsAPIStoresConfigOpaquely(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	const config = `{"team_id":"T1","channels":["C1"],"anything":42}`
	body := `{"name":"ios-bot","kind":"slack","config":` + config + `}`
	resp := do(t, "POST", base+"/v1/bots", body, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var got struct {
		ID     string         `json:"id"`
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Config["anything"] != float64(42) {
		t.Fatalf("config was reshaped: %v", got.Config)
	}
	if !bytes.Equal(bytes.TrimSpace(fake.created.Config), []byte(config)) {
		t.Fatalf("the store seam received %q, want the caller's config verbatim", fake.created.Config)
	}
}

// TestBotsAPICarriesNoSlackSymbol is the structural guard the brief demands: this file must never name a
// channel. kind is a string this package treats as opaque, never a special case.
func TestBotsAPICarriesNoSlackSymbol(t *testing.T) {
	src, err := os.ReadFile("bots.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ToLower(src), []byte("slack")) {
		t.Fatal("bots.go mentions slack; the registry must stay kind-agnostic")
	}
}

// TestBotsCreateTakesTenantFromTheVerifiedScope pins the §39.2 rule every registration surface in this
// tree carries: the row lands in the BEARER's org/project, never a field the body could name.
func TestBotsCreateTakesTenantFromTheVerifiedScope(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/bots", `{"name":"ios-bot","kind":"slack"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/bots/bot_1" {
		t.Fatalf("Location = %q, want /v1/bots/bot_1", loc)
	}
	if fake.createdOrg != "org_1" || fake.createdProject != "prj_1" {
		t.Fatalf("bot created in (%s, %s), want the verified scope (org_1, prj_1)", fake.createdOrg, fake.createdProject)
	}
}

// TestBotsCreateRequiresNameAndKind pins the 400/Invalid mapping — a bot with neither identifies nothing
// and speaks as nothing.
func TestBotsCreateRequiresNameAndKind(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no name", `{"kind":"slack"}`},
		{"no kind", `{"name":"ios-bot"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeBotRegistry{}
			base := botTestServer(t, fake)
			resp := do(t, "POST", base+"/v1/bots", tc.body, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
		})
	}
}

// TestBotsCreateRejectsUnknownField pins DisallowUnknownFields at the boundary: a caller cannot smuggle a
// tenant field (or anything else) past the create body and have it silently dropped.
func TestBotsCreateRejectsUnknownField(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/bots", `{"name":"ios-bot","kind":"slack","organization_id":"org_victim"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fake.createdOrg != "" {
		t.Fatalf("a body refused at the boundary still reached the store seam")
	}
}

// TestBotsCreateMapsNameConflict pins the 409 mapping for the registry's UNIQUE (org, project, name).
func TestBotsCreateMapsNameConflict(t *testing.T) {
	fake := &fakeBotRegistry{createErr: ErrBotNameTaken}
	base := botTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/bots", `{"name":"ios-bot","kind":"slack"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("status = %d, want 409", resp.StatusCode)
	}
}

// TestBotsListIsScopedAndPaged pins the shared keyset envelope and the verified-scope rule on the read side.
func TestBotsListIsScopedAndPaged(t *testing.T) {
	row, _ := json.Marshal(map[string]any{"id": "bot_1", "object": "bot", "name": "ios-bot", "kind": "slack"})
	fake := &fakeBotRegistry{items: []ListRow{{ID: "bot_1", Body: row}}}
	base := botTestServer(t, fake)
	resp := do(t, "GET", base+"/v1/bots?limit=5", "", nil)
	page := assertPageLen(t, resp, 1)
	if page.Data[0].(map[string]any)["id"] != "bot_1" {
		t.Fatalf("page row = %v, want id bot_1", page.Data[0])
	}
	if fake.listedOrg != "org_1" || fake.listedProject != "prj_1" {
		t.Fatalf("listed (%s, %s), want the verified scope (org_1, prj_1)", fake.listedOrg, fake.listedProject)
	}
	if fake.lastQuery.Limit != 5 {
		t.Fatalf("limit = %d, want 5", fake.lastQuery.Limit)
	}
}

// TestBotsGetUnknownIsNotFound pins the non-disclosing 404 for a foreign or unknown id.
func TestBotsGetUnknownIsNotFound(t *testing.T) {
	fake := &fakeBotRegistry{missing: true}
	base := botTestServer(t, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_other", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// TestBotsGetKnownReturnsTheRowShape pins the plan's row shape verbatim.
func TestBotsGetKnownReturnsTheRowShape(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "GET", base+"/v1/bots/bot_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"id", "name", "kind", "agent_revision_id", "repository_binding_id", "config", "disabled"} {
		if _, ok := got[field]; !ok {
			t.Fatalf("row is missing %q: %v", field, got)
		}
	}
}

// TestBotsPatchRepairsUnderTheVerifiedScopeAndLeavesTheRestAlone pins the partial-revision contract: only
// the mentioned field reaches the store as non-nil.
func TestBotsPatchRepairsUnderTheVerifiedScopeAndLeavesTheRestAlone(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "PATCH", base+"/v1/bots/bot_1", `{"agent_revision_id":"rev_1"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if fake.patchedID != "bot_1" || fake.patchOrg != "org_1" {
		t.Fatalf("patch scope = %s/%s id=%s, want org_1/bot_1", fake.patchOrg, fake.patchProject, fake.patchedID)
	}
	if fake.patched.AgentRevisionID == nil || *fake.patched.AgentRevisionID != "rev_1" {
		t.Fatalf("agent_revision_id = %v, want rev_1", fake.patched.AgentRevisionID)
	}
	if fake.patched.Name != nil || fake.patched.RepositoryBindingID != nil || fake.patched.PrincipalID != nil ||
		fake.patched.Disabled != nil || fake.patched.Config != nil {
		t.Fatalf("an unmentioned field was set: %+v", fake.patched)
	}
}

// TestBotsPatchCannotChangeKind pins kind's immutability structurally: DisallowUnknownFields refuses the
// field outright, so there is no statement by which a bot could be moved onto a different channel.
func TestBotsPatchCannotChangeKind(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "PATCH", base+"/v1/bots/bot_1", `{"kind":"whatsapp"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fake.patchedID != "" {
		t.Fatalf("a kind change reached the store; it must be refused at the boundary")
	}
}

// TestBotsPatchNameCannotBeCleared pins the same "a revise may not reach a state a create forbids" rule
// this tree's other registration surfaces enforce.
func TestBotsPatchNameCannotBeCleared(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "PATCH", base+"/v1/bots/bot_1", `{"name":""}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	if fake.patchedID != "" {
		t.Fatalf("a name-clearing patch reached the store; it must be refused at the boundary")
	}
}

// TestBotsDeleteRemovesItAndAnUnknownIsNotFound pins the DELETE contract.
func TestBotsDeleteRemovesItAndAnUnknownIsNotFound(t *testing.T) {
	fake := &fakeBotRegistry{}
	base := botTestServer(t, fake)
	resp := do(t, "DELETE", base+"/v1/bots/bot_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
	if fake.deletedID != "bot_1" || fake.deleteOrg != "org_1" {
		t.Fatalf("delete = %q under %q, want bot_1 under org_1", fake.deletedID, fake.deleteOrg)
	}

	fake2 := &fakeBotRegistry{missing: true}
	base2 := botTestServer(t, fake2)
	resp2 := do(t, "DELETE", base2+"/v1/bots/bot_other", "", nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp2.StatusCode)
	}
}

// TestBotsRoutesAreUnmountedWithoutTheOption: a tier that wires no bot store must 404 rather than 500 on a
// nil seam — the posture every other optional surface in this router takes.
func TestBotsRoutesAreUnmountedWithoutTheOption(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	defer srv.Close()
	for _, m := range []string{"POST", "GET"} {
		resp := do(t, m, srv.URL+"/v1/bots", `{"name":"x","kind":"slack"}`, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with no WithBots = %d, want 404", m, resp.StatusCode)
		}
	}
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		resp := do(t, m, srv.URL+"/v1/bots/bot_1", `{"name":"x"}`, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s by id with no WithBots = %d, want 404", m, resp.StatusCode)
		}
	}
}
