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
}
