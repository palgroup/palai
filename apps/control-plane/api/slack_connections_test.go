package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeSlackConnectionAPI records what reached the store seam, and can be told to fail with any of the
// store's typed sentinels so the handler's status mapping is pinned against the REAL errors rather than
// against a stand-in the handler could not actually receive.
type fakeSlackConnectionAPI struct {
	createdOrg, createdProject string
	createdBody                []byte
	createErr                  error
	items                      []SlackConnectionItem
	lastWindow                 SlackListWindow
	listedOrg, listedProject   string

	// The repair half. missing makes every by-id call report not-found, so the 404 mapping is pinned.
	detail                   SlackConnectionDetail
	missing                  bool
	patched                  SlackConnectionPatch
	patchedID, deletedID     string
	repairOrg, repairProject string
}

func (f *fakeSlackConnectionAPI) CreateSlackConnection(_ context.Context, org, project string, raw []byte) (string, error) {
	f.createdOrg, f.createdProject, f.createdBody = org, project, raw
	if f.createErr != nil {
		return "", f.createErr
	}
	return "slkc_1", nil
}

func (f *fakeSlackConnectionAPI) ListSlackConnections(_ context.Context, org, project string, w SlackListWindow) ([]SlackConnectionItem, error) {
	f.listedOrg, f.listedProject, f.lastWindow = org, project, w
	return f.items, nil
}

func (f *fakeSlackConnectionAPI) GetSlackConnection(_ context.Context, org, project, id string) (SlackConnectionDetail, bool, error) {
	f.repairOrg, f.repairProject = org, project
	if f.missing {
		return SlackConnectionDetail{}, false, nil
	}
	d := f.detail
	d.ID = id
	return d, true, nil
}

func (f *fakeSlackConnectionAPI) UpdateSlackConnection(_ context.Context, org, project, id string, patch SlackConnectionPatch) (bool, error) {
	f.repairOrg, f.repairProject, f.patchedID, f.patched = org, project, id, patch
	return !f.missing, nil
}

func (f *fakeSlackConnectionAPI) DeleteSlackConnection(_ context.Context, org, project, id string) (bool, error) {
	f.repairOrg, f.repairProject, f.deletedID = org, project, id
	return !f.missing, nil
}

func slackConnTestServer(t *testing.T, s SlackConnectionAPI) string {
	t.Helper()
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithSlackConnections(s)))
	t.Cleanup(srv.Close)
	return srv.URL
}

const goodSlackBody = `{"team_id":"T1","bot_user_id":"Ubot","signing_secret_ref":"slack/signing",
	"bot_token_ref":"slack/bot","app_token_ref":"slack/app",
	"default_policy":{"agent_revision_id":"arev_1","principal_id":"prin_1"}}`

// TestSlackConnectionCreateTakesTenantFromTheVerifiedScope is the §39.2 pin on the registration side: the
// workspace binding lands in the BEARER's org/project and a body that names another tenant changes nothing.
// It is the same rule the inbound receiver enforces on the event side — a payload never selects a tenant.
func TestSlackConnectionCreateTakesTenantFromTheVerifiedScope(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)

	resp := do(t, "POST", base+"/v1/slack-connections", goodSlackBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/v1/slack-connections/slkc_1" {
		t.Fatalf("Location = %q, want /v1/slack-connections/slkc_1", loc)
	}
	if fake.createdOrg != "org_1" || fake.createdProject != "prj_1" {
		t.Fatalf("connection created in (%s, %s), want the verified scope (org_1, prj_1)", fake.createdOrg, fake.createdProject)
	}
}

// TestSlackConnectionCreateRefusesARawCredential is the one refusal this surface exists to make. The whole
// point of registering a workspace over the API rather than by hand-written SQL is that the secret material
// arrives as secret_ref HANDLES; a body carrying an inline `signing_secret` / `bot_token` / `app_token`
// VALUE must be refused AT THE EDGE, so the value never reaches a store call, a log line or a pgx argument
// list in the first place.
// Each body below is OTHERWISE COMPLETE — it would be accepted if the inline field were removed — so the
// only thing that can refuse it is the unknown-field guard. An earlier draft used bodies that were also
// missing default_policy, and they passed against an implementation with the guard DELETED: green for a
// reason that had nothing to do with the claim. The control is TestSlackConnectionCreateAcceptsTheHandleOnly
// body, which is these bodies minus the inline field.
func TestSlackConnectionCreateRefusesARawCredential(t *testing.T) {
	const policy = `"default_policy":{"agent_revision_id":"arev_1","principal_id":"prin_1"}`
	for _, tc := range []struct{ name, body string }{
		{"inline signing secret", `{"team_id":"T1","signing_secret_ref":"r","signing_secret":"8f2a0b",` + policy + `}`},
		{"inline bot token", `{"team_id":"T1","signing_secret_ref":"r","bot_token":"xoxb-real",` + policy + `}`},
		{"inline app token", `{"team_id":"T1","signing_secret_ref":"r","app_token":"xapp-real",` + policy + `}`},
		{"a tenant field", `{"team_id":"T1","signing_secret_ref":"r","organization_id":"org_victim",` + policy + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSlackConnectionAPI{}
			base := slackConnTestServer(t, fake)
			resp := do(t, "POST", base+"/v1/slack-connections", tc.body, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400 — an inline credential must be refused at the boundary", resp.StatusCode)
			}
			if fake.createdBody != nil {
				t.Fatalf("the inline credential reached the store seam (%q); it must be refused before any store call", fake.createdBody)
			}
		})
	}
}

// TestSlackConnectionCreateAcceptsTheHandleOnlyBody is the CONTROL for the test above: the same bodies with
// the inline field removed are ACCEPTED. Without it, "every body was refused" would satisfy the negatives
// just as well as a working guard would.
func TestSlackConnectionCreateAcceptsTheHandleOnlyBody(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/slack-connections",
		`{"team_id":"T1","signing_secret_ref":"r","bot_token_ref":"b","app_token_ref":"a",
		  "default_policy":{"agent_revision_id":"arev_1","principal_id":"prin_1"}}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("the handle-only body = %d, want 201 — the guard must discriminate, not refuse everything", resp.StatusCode)
	}
	// The bytes handed to the store are the CANONICAL re-marshal, not the caller's. That is what makes the
	// unknown-field refusal load-bearing: nothing the edge rejected can ride along in the original body.
	var forwarded map[string]any
	if err := json.Unmarshal(fake.createdBody, &forwarded); err != nil {
		t.Fatalf("the store seam did not receive JSON: %v", err)
	}
	for key := range forwarded {
		switch key {
		case "team_id", "enterprise_id", "bot_user_id", "signing_secret_ref", "bot_token_ref",
			"app_token_ref", "scopes", "allowed_channels", "allowed_users", "default_policy":
		default:
			t.Fatalf("the store seam received an unexpected key %q", key)
		}
	}
}

// TestSlackConnectionCreateRejectsAtTheBoundary pins the rest of the refusals, including the one that is a
// security property rather than tidiness: default_policy is tenant-supplied JSONB the admission bridge reads
// a RUN TARGET out of, so an open shape is exactly where a second "organization_id" or a bearer token would
// be parked. Only the two keys slack_admit.go actually reads are allowed.
func TestSlackConnectionCreateRejectsAtTheBoundary(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"no team id", `{"signing_secret_ref":"r","default_policy":{"agent_revision_id":"a","principal_id":"p"}}`},
		{"no signing secret ref", `{"team_id":"T1","default_policy":{"agent_revision_id":"a","principal_id":"p"}}`},
		{"no default policy", `{"team_id":"T1","signing_secret_ref":"r"}`},
		{"policy without a revision", `{"team_id":"T1","signing_secret_ref":"r","default_policy":{"principal_id":"p"}}`},
		{"policy without a principal", `{"team_id":"T1","signing_secret_ref":"r","default_policy":{"agent_revision_id":"a"}}`},
		{"policy with an unknown key", `{"team_id":"T1","signing_secret_ref":"r","default_policy":{"agent_revision_id":"a","principal_id":"p","organization_id":"org_victim"}}`},
		{"not json", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeSlackConnectionAPI{}
			base := slackConnTestServer(t, fake)
			resp := do(t, "POST", base+"/v1/slack-connections", tc.body, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", resp.StatusCode)
			}
			if fake.createdBody != nil {
				t.Fatalf("a body refused at the boundary still reached the store seam: %q", fake.createdBody)
			}
		})
	}
}

// TestSlackConnectionCreateMapsTheStoreRefusals is the E19 T1 cross-tenant hijack fix surviving THROUGH the
// new surface. CreateSlackConnection refuses a workspace already bound in another tenant
// (ErrSlackWorkspaceBoundElsewhere); a handler that mapped that to a 500 would turn a deliberate security
// refusal into "try again later", and one that echoed the error would tell the registering admin that
// another customer holds the workspace. Both are asserted: 409, and a body naming NO tenant.
func TestSlackConnectionCreateMapsTheStoreRefusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"already bound in this project", ErrSlackRegistrationConflict, http.StatusConflict},
		{"bound by another tenant", ErrSlackRegistrationConflict, http.StatusConflict},
		{"invalid config", ErrSlackRegistrationInvalid, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := slackConnTestServer(t, &fakeSlackConnectionAPI{createErr: tc.err})
			resp := do(t, "POST", base+"/v1/slack-connections", goodSlackBody, nil)
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			var body map[string]any
			_ = json.NewDecoder(resp.Body).Decode(&body)
			detail, _ := body["detail"].(string)
			for _, leak := range []string{"org_", "prj_", "slkc_"} {
				if strings.Contains(detail, leak) {
					t.Fatalf("the refusal detail %q names another tenant's identifiers; a registering admin must not learn that another customer exists", detail)
				}
			}
		})
	}
}

// TestSlackConnectionListIsScopedAndCarriesNoSecretRef: the list is the operator's way to find the id of
// the workspace they just registered. It is tenant-scoped from the verified bearer, and it returns NO
// secret-ref handles — a list is a browse surface and a handle is not needed to browse.
func TestSlackConnectionListIsScopedAndCarriesNoSecretRef(t *testing.T) {
	fake := &fakeSlackConnectionAPI{items: []SlackConnectionItem{
		{ID: "slkc_1", TeamID: "T1", BotUserID: "Ubot"},
	}}
	base := slackConnTestServer(t, fake)

	resp := do(t, "GET", base+"/v1/slack-connections", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("list status = %d, want 200", resp.StatusCode)
	}
	if fake.listedOrg != "org_1" || fake.listedProject != "prj_1" {
		t.Fatalf("listed (%s, %s), want the verified scope (org_1, prj_1)", fake.listedOrg, fake.listedProject)
	}
	var page struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatalf("decode page: %v", err)
	}
	if len(page.Data) != 1 || page.HasMore {
		t.Fatalf("page = %+v, want the shared keyset envelope with one row", page)
	}
	if page.Data[0]["object"] != "slack_connection" {
		t.Fatalf("row object = %v, want slack_connection", page.Data[0]["object"])
	}
	for key := range page.Data[0] {
		if strings.Contains(key, "ref") || strings.Contains(key, "secret") || strings.Contains(key, "token") {
			t.Fatalf("the list row carries %q; a browse surface has no use for secret material or its handles", key)
		}
	}
}

// TestSlackConnectionRoutesAreUnmountedWithoutTheOption: a tier that wires no Slack store must 404 rather
// than 500 on a nil seam — the same posture every other optional surface takes.
func TestSlackConnectionRoutesAreUnmountedWithoutTheOption(t *testing.T) {
	srv := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	defer srv.Close()
	for _, m := range []string{"POST", "GET"} {
		resp := do(t, m, srv.URL+"/v1/slack-connections", goodSlackBody, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s with no WithSlackConnections = %d, want 404", m, resp.StatusCode)
		}
	}
	// The by-id repair routes are mounted on the same nil check, so they must be absent too.
	for _, m := range []string{"GET", "PATCH", "DELETE"} {
		resp := do(t, m, srv.URL+"/v1/slack-connections/slkc_1", `{"scopes":"chat:write"}`, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s by id with no WithSlackConnections = %d, want 404", m, resp.StatusCode)
		}
	}
}

// A binding registered before a field existed — an app_token_ref on a connection created before Socket Mode
// needed one — was previously unrepairable through any API: the operator's only route was raw SQL against
// slack_connections. This is that repair, and the tenant rule is the create path's: the scope comes from the
// verified bearer, never from the path or the body.
func TestSlackConnectionPatchRepairsAHandleUnderTheVerifiedScope(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)

	resp := do(t, "PATCH", base+"/v1/slack-connections/slkc_1", `{"app_token_ref":"slack/app"}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("patch status = %d, want 200", resp.StatusCode)
	}
	if fake.patchedID != "slkc_1" {
		t.Fatalf("patched id = %q, want slkc_1", fake.patchedID)
	}
	if fake.repairOrg != "org_1" || fake.repairProject != "prj_1" {
		t.Fatalf("patch scope = %s/%s, want the bearer's org_1/prj_1", fake.repairOrg, fake.repairProject)
	}
	if fake.patched.AppTokenRef == nil || *fake.patched.AppTokenRef != "slack/app" {
		t.Fatalf("app_token_ref = %v, want slack/app", fake.patched.AppTokenRef)
	}
	// Everything the body did not mention stays nil, so the statement COALESCEs it to the stored value: a
	// PATCH that repairs one handle must not silently blank the other eight columns.
	if fake.patched.BotTokenRef != nil || fake.patched.SigningSecretRef != nil || fake.patched.Disabled != nil ||
		fake.patched.AllowedChannels != nil || fake.patched.AllowedUsers != nil || fake.patched.DefaultPolicy != nil {
		t.Fatalf("an unmentioned field was set: %+v", fake.patched)
	}
}

// The squat refusal survives the new surface STRUCTURALLY: the workspace a binding points at is not a
// revisable field, so there is no request that moves a connection onto a team someone else holds — the
// registration-time check cannot be routed around because there is nothing to route.
func TestSlackConnectionPatchCannotMoveTheWorkspaceOrTheTenant(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)

	for _, body := range []string{
		`{"team_id":"T_victim"}`,
		`{"enterprise_id":"E_victim"}`,
		`{"organization_id":"org_other"}`,
		`{"project_id":"proj_other"}`,
		`{"signing_secret":"8f742231b10e"}`,
		`{"bot_token":"xoxb-real-token"}`,
		`{"signing_secret_ref":""}`,
		`{"default_policy":{"agent_revision_id":"arev_1"}}`,
		`{"default_policy":{"agent_revision_id":"arev_1","principal_id":"p1","organization_id":"org_other"}}`,
	} {
		resp := do(t, "PATCH", base+"/v1/slack-connections/slkc_1", body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("PATCH %s = %d, want 400", body, resp.StatusCode)
		}
		if fake.patchedID != "" {
			t.Fatalf("PATCH %s reached the store; it must be refused at the boundary", body)
		}
	}
}

// A connection in ANOTHER tenant answers 404 on every by-id verb — never 403, which would confirm it exists.
func TestSlackConnectionRepairOfAForeignIDIsNotFound(t *testing.T) {
	fake := &fakeSlackConnectionAPI{missing: true}
	base := slackConnTestServer(t, fake)

	for _, tc := range []struct{ method, body string }{
		{"GET", ""}, {"PATCH", `{"scopes":"chat:write"}`}, {"DELETE", ""},
	} {
		resp := do(t, tc.method, base+"/v1/slack-connections/slkc_other", tc.body, nil)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s of a foreign connection = %d, want 404", tc.method, resp.StatusCode)
		}
	}
}

// The read half of the repair loop: an operator cannot fix a handle they cannot see, and the LIST projection
// deliberately omits every ref. So the by-id read carries the handles — which are names, never values.
func TestSlackConnectionGetCarriesTheHandlesAndNoSecretValues(t *testing.T) {
	fake := &fakeSlackConnectionAPI{detail: SlackConnectionDetail{
		TeamID: "T1", BotUserID: "Ubot", SigningSecretRef: "slack/signing", BotTokenRef: "slack/bot", AppTokenRef: "",
	}}
	base := slackConnTestServer(t, fake)

	resp := do(t, "GET", base+"/v1/slack-connections/slkc_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["signing_secret_ref"] != "slack/signing" || got["app_token_ref"] != "" {
		t.Fatalf("read = %v, want the handles including the EMPTY app_token_ref an operator has to notice", got)
	}
	for key := range got {
		if key == "signing_secret" || key == "bot_token" || key == "app_token" {
			t.Fatalf("the read carries %q; only handles exist on this surface", key)
		}
	}
}

// DELETE answers 204 with no body — the schedules posture.
func TestSlackConnectionDeleteUnbindsTheWorkspace(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)

	resp := do(t, "DELETE", base+"/v1/slack-connections/slkc_1", "", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if fake.deletedID != "slkc_1" || fake.repairOrg != "org_1" {
		t.Fatalf("delete = %q under %q, want slkc_1 under the bearer's org", fake.deletedID, fake.repairOrg)
	}
}

// ============================================================================
// E22 T3: the repository half of default_policy.
// ============================================================================
//
// A Slack-born run was STRUCTURALLY non-coding until this shape grew: nothing a connection could say named a
// repository, so the admission bridge left RepositoryBindingID empty and the run provisioned no workspace.
// The two fields are the WHOLE of that change on this surface — and the interesting requirement is not that
// they are accepted, it is that the struct did not open and that a connection which names neither is
// byte-for-byte the connection it was before.

// TestSlackDefaultPolicyAcceptsARepositoryAndStaysClosed: the two new keys are accepted and canonicalised,
// and an unknown key beside them is still a 400. Growing a closed shape must not be the same as opening it.
func TestSlackDefaultPolicyAcceptsARepositoryAndStaysClosed(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/slack-connections",
		`{"team_id":"T1","signing_secret_ref":"r","default_policy":{"agent_revision_id":"arev_1",
		  "principal_id":"prin_1","repository_binding_id":"repo_1","repository_ref":"feature/x"}}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("a policy naming a repository = %d, want 201", resp.StatusCode)
	}
	var forwarded struct {
		DefaultPolicy slackDefaultPolicy `json:"default_policy"`
	}
	if err := json.Unmarshal(fake.createdBody, &forwarded); err != nil {
		t.Fatalf("the store seam did not receive JSON: %v", err)
	}
	if forwarded.DefaultPolicy.RepositoryBindingID != "repo_1" || forwarded.DefaultPolicy.RepositoryRef != "feature/x" {
		t.Fatalf("the repository did not reach the store: %+v", forwarded.DefaultPolicy)
	}

	// The closure is still real: a key beside the four is refused, exactly as it was when there were two.
	fake = &fakeSlackConnectionAPI{}
	base = slackConnTestServer(t, fake)
	resp2 := do(t, "POST", base+"/v1/slack-connections",
		`{"team_id":"T1","signing_secret_ref":"r","default_policy":{"agent_revision_id":"a","principal_id":"p",
		  "repository_binding_id":"repo_1","clone_url":"https://evil.test/x.git"}}`, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("an unknown key beside the repository = %d, want 400 — a destination must never be settable "+
			"from a policy document, only resolvable from a registered binding", resp2.StatusCode)
	}
	if fake.createdBody != nil {
		t.Fatalf("the refused body still reached the store: %q", fake.createdBody)
	}
}

// TestSlackDefaultPolicyWithoutARepositoryIsBitUnchanged is the compatibility claim, asserted on the BYTES
// rather than on behaviour: a registration that names no repository must hand the store the same two-key
// document it always did. `omitempty` is what makes that true, and this test is what makes it stay true.
func TestSlackDefaultPolicyWithoutARepositoryIsBitUnchanged(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/slack-connections", goodSlackBody, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("status = %d, want 201", resp.StatusCode)
	}
	var forwarded struct {
		DefaultPolicy json.RawMessage `json:"default_policy"`
	}
	if err := json.Unmarshal(fake.createdBody, &forwarded); err != nil {
		t.Fatalf("the store seam did not receive JSON: %v", err)
	}
	const want = `{"agent_revision_id":"arev_1","principal_id":"prin_1"}`
	if string(forwarded.DefaultPolicy) != want {
		t.Fatalf("canonical default_policy = %s, want %s — a connection that binds no repository must be "+
			"byte-identical to the one it was before E22, or every stored row moves for a feature it does not use",
			forwarded.DefaultPolicy, want)
	}
}

// TestSlackDefaultPolicyRefusesARefWithNoBinding: a ref with nothing to check it out of would be accepted,
// stored, and ignored forever — the settable-but-inert field this repository keeps paying for (a tool no list
// named, a handle that resolved nowhere). Refused on BOTH write paths, since a revise may not reach a state a
// create forbids.
func TestSlackDefaultPolicyRefusesARefWithNoBinding(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)
	resp := do(t, "POST", base+"/v1/slack-connections",
		`{"team_id":"T1","signing_secret_ref":"r","default_policy":{"agent_revision_id":"a","principal_id":"p",
		  "repository_ref":"feature/x"}}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("a ref with no binding = %d, want 400", resp.StatusCode)
	}
	if fake.createdBody != nil {
		t.Fatalf("the refused body still reached the store: %q", fake.createdBody)
	}
	patch := do(t, "PATCH", base+"/v1/slack-connections/slkc_1",
		`{"default_policy":{"agent_revision_id":"a","principal_id":"p","repository_ref":"feature/x"}}`, nil)
	defer patch.Body.Close()
	if patch.StatusCode != http.StatusBadRequest {
		t.Fatalf("a ref with no binding on PATCH = %d, want 400 — a revise may not widen what a create refused",
			patch.StatusCode)
	}
	if fake.patchedID != "" {
		t.Fatalf("the refused revision reached the store as %q", fake.patchedID)
	}
}

// The repair path accepts the repository too: a workspace bound before E22 must be able to GAIN one without
// being deleted and re-registered — the "a surface without a revise can only ever be wrong once" rule that
// this file's repair half exists for.
func TestSlackDefaultPolicyPatchBindsARepositoryToAnExistingConnection(t *testing.T) {
	fake := &fakeSlackConnectionAPI{}
	base := slackConnTestServer(t, fake)
	resp := do(t, "PATCH", base+"/v1/slack-connections/slkc_1",
		`{"default_policy":{"agent_revision_id":"arev_1","principal_id":"prin_1","repository_binding_id":"repo_1"}}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	const want = `{"agent_revision_id":"arev_1","principal_id":"prin_1","repository_binding_id":"repo_1"}`
	if string(fake.patched.DefaultPolicy) != want {
		t.Fatalf("canonical patched policy = %s, want %s", fake.patched.DefaultPolicy, want)
	}
}
