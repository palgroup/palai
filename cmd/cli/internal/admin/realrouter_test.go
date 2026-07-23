package admin

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	capi "github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/api/middleware"
)

// This file proves the admin CLI against the GERÇEK control-plane router in-process (Docker-free, no DB):
// api.NewRouter with fake stores + the real auth/provision-gate middleware. It catches what a permissive
// stub cannot — a wrong route/method against the real mux, the real 401/403 gate, and the real RFC9457
// problem shape — which the CLI must render.

// fakeProv implements api.ProvisioningAPI with canned per-method bodies, so the CLI round-trips every
// provisioning route through the real router.
type fakeProv struct{}

func (fakeProv) CreateOrganization(context.Context, middleware.Scope, []byte) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"org_new","object":"organization","display_name":"Acme","default_project_id":"prj_new","admin_api_key":{"id":"key_new","object":"api_key","key":"sk_oneTimeOrgKey"}}`)}, nil
}
func (fakeProv) ListOrganizations(context.Context, middleware.Scope) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"object":"list","data":[{"id":"org_1","object":"organization"}]}`)}, nil
}
func (fakeProv) GetOrganization(context.Context, middleware.Scope, string) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"org_1","object":"organization"}`)}, nil
}
func (fakeProv) CreateProject(context.Context, middleware.Scope, []byte) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"prj_new","object":"project"}`)}, nil
}
func (fakeProv) ListProjects(context.Context, middleware.Scope) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}, nil
}
func (fakeProv) GetProject(context.Context, middleware.Scope, string) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"prj_1","object":"project"}`)}, nil
}
func (fakeProv) UpdateProjectPolicy(_ context.Context, _ middleware.Scope, _ string, body []byte) (capi.ProvisionResult, error) {
	// Echo the policy the CLI sent so the test can assert set-policy reached the real PATCH route intact.
	return capi.ProvisionResult{Body: append([]byte(`{"id":"prj_1","object":"project","received":`), append(body, '}')...)}, nil
}
func (fakeProv) CreateAPIKey(context.Context, middleware.Scope, []byte) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"key_new","object":"api_key","key":"sk_oneTimeApiKey"}`)}, nil
}
func (fakeProv) ListAPIKeys(context.Context, middleware.Scope) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"object":"list","data":[]}`)}, nil
}
func (fakeProv) GetAPIKey(context.Context, middleware.Scope, string) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"key_1","object":"api_key"}`)}, nil
}
func (fakeProv) RevokeAPIKey(context.Context, middleware.Scope, string) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"id":"key_1","object":"api_key","revoked_at":"2026-07-23T00:00:00Z"}`)}, nil
}

// fakeSec implements api.SecretRefAPI. It records the last write body so the test can prove the stdin value
// reached the store through the real router — while every response stays metadata-only.
type fakeSec struct{ lastWrite []byte }

func (f *fakeSec) CreateSecretRef(_ context.Context, _ middleware.Scope, body []byte) (capi.ProvisionResult, error) {
	f.lastWrite = body
	return capi.ProvisionResult{Body: []byte(`{"name":"db-url","object":"secret_ref","version":1}`)}, nil
}
func (f *fakeSec) ListSecretRefs(context.Context, middleware.Scope) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"object":"list","data":[{"name":"db-url","object":"secret_ref","version":1}]}`)}, nil
}
func (f *fakeSec) GetSecretRef(context.Context, middleware.Scope, string) (capi.ProvisionResult, error) {
	return capi.ProvisionResult{Body: []byte(`{"name":"db-url","object":"secret_ref","version":1}`)}, nil
}
func (f *fakeSec) RotateSecretRef(_ context.Context, _ middleware.Scope, _ string, body []byte) (capi.ProvisionResult, error) {
	f.lastWrite = body
	return capi.ProvisionResult{Body: []byte(`{"name":"db-url","object":"secret_ref","version":2}`)}, nil
}

// staticVerifier resolves every bearer to a fixed scope. err != nil drives the real 401 path.
type staticVerifier struct {
	scope middleware.Scope
	err   error
}

func (v staticVerifier) VerifyAPIKey(context.Context, string) (middleware.Scope, error) {
	return v.scope, v.err
}

func realRouterServer(t *testing.T, v middleware.Verifier, sec capi.SecretRefAPI) string {
	t.Helper()
	h := capi.NewRouter(v, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, fakeProv{}, nil, capi.SSEConfig{}, nil, nil, capi.WithSecretRefs(sec))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv.URL
}

// TestAdminCLIAgainstRealRouter drives every subcommand through the real router + admin key (empty scopes),
// proving the CLI hits routes the real mux actually serves, that the one-time key is disclosed on create,
// and that the write-only secret value reaches the store but never the response.
func TestAdminCLIAgainstRealRouter(t *testing.T) {
	sec := &fakeSec{}
	base := realRouterServer(t, staticVerifier{scope: middleware.Scope{Organization: "org_1", Project: "prj_1"}}, sec)
	t.Setenv("PALAI_BASE_URL", base)
	t.Setenv("PALAI_API_KEY", "bootstrap-admin-key")

	type step struct {
		args       []string
		stdin      string
		wantOut    string // substring the rendered success body must contain
		wantNotOut string // substring the output must NOT contain (empty = skip)
	}
	steps := []step{
		{args: []string{"org", "create", "--display-name", "Acme"}, wantOut: `"sk_oneTimeOrgKey"`, wantNotOut: "bootstrap-admin-key"},
		{args: []string{"org", "list"}, wantOut: `"org_1"`},
		{args: []string{"org", "get", "org_1"}, wantOut: `"organization"`},
		{args: []string{"project", "create", "--display-name", "P"}, wantOut: `"prj_new"`},
		{args: []string{"project", "list"}, wantOut: `"list"`},
		{args: []string{"project", "get", "prj_1"}, wantOut: `"prj_1"`},
		{args: []string{"project", "set-policy", "prj_1", "--allowed-models", "m1,m2"}, wantOut: `"m1"`},
		{args: []string{"apikey", "create", "--project", "prj_1"}, wantOut: `"sk_oneTimeApiKey"`, wantNotOut: "bootstrap-admin-key"},
		{args: []string{"apikey", "list"}, wantOut: `"list"`},
		{args: []string{"apikey", "get", "key_1"}, wantOut: `"key_1"`},
		{args: []string{"apikey", "revoke", "key_1"}, wantOut: `"revoked_at"`},
		{args: []string{"secret", "create", "--name", "db-url"}, stdin: "postgres://the-secret", wantOut: `"secret_ref"`, wantNotOut: "the-secret"},
		{args: []string{"secret", "list"}, wantOut: `"db-url"`},
		{args: []string{"secret", "get", "db-url"}, wantOut: `"secret_ref"`},
		{args: []string{"secret", "rotate", "db-url"}, stdin: "postgres://rotated", wantOut: `"version": 2`, wantNotOut: "rotated"},
	}
	for _, s := range steps {
		t.Run(strings.Join(s.args, " "), func(t *testing.T) {
			var out bytes.Buffer
			if err := Run(s.args[0], s.args[1:], &out, strings.NewReader(s.stdin)); err != nil {
				t.Fatalf("Run(%v): %v", s.args, err)
			}
			if s.wantOut != "" && !strings.Contains(out.String(), s.wantOut) {
				t.Errorf("output %q missing %q", out.String(), s.wantOut)
			}
			if s.wantNotOut != "" && strings.Contains(out.String(), s.wantNotOut) {
				t.Errorf("output %q leaked %q", out.String(), s.wantNotOut)
			}
		})
	}

	// The write-only value reached the store through the real POST/rotate routes, though no response echoed it.
	if !strings.Contains(string(sec.lastWrite), "postgres://rotated") {
		t.Fatalf("secret value did not reach the store via the real router: %q", sec.lastWrite)
	}
}

// TestAdminCLIRealRouterInsufficientScope drives the real provision-capability gate: a run-only key is
// refused with a 403 whose RFC9457 problem the CLI renders (code + request id).
func TestAdminCLIRealRouterInsufficientScope(t *testing.T) {
	base := realRouterServer(t, staticVerifier{scope: middleware.Scope{Organization: "org_1", Scopes: []string{"run"}}}, &fakeSec{})
	t.Setenv("PALAI_BASE_URL", base)
	t.Setenv("PALAI_API_KEY", "run-only-key")

	err := Run("org", []string{"list"}, new(bytes.Buffer), strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("want an insufficient_scope render, got %v", err)
	}
}

// TestAdminCLIRealRouterInvalidToken drives the real auth middleware: a bearer the verifier rejects is a
// 401 the CLI renders as invalid_token.
func TestAdminCLIRealRouterInvalidToken(t *testing.T) {
	base := realRouterServer(t, staticVerifier{err: middleware.ErrInvalidToken}, &fakeSec{})
	t.Setenv("PALAI_BASE_URL", base)
	t.Setenv("PALAI_API_KEY", "bogus-key")

	err := Run("org", []string{"list"}, new(bytes.Buffer), strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "invalid_token") {
		t.Fatalf("want an invalid_token render, got %v", err)
	}
}
