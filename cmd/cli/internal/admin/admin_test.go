package admin

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// capture records the one request a stub server saw, so a test can assert the CLI built the
// right method, path, authorization, and body for a subcommand.
type capture struct {
	method, path, auth, body string
}

// stubServer answers every request with (status, body) and records what it saw. It stands in
// for the real control-plane: the CLI is a thin client, so pinning method/path/headers/body
// against a stub proves the whole client contract without a database.
func stubServer(t *testing.T, status int, respBody string, cap *capture) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		*cap = capture{method: r.Method, path: r.URL.Path, auth: r.Header.Get("Authorization"), body: string(b)}
		if status >= 400 {
			w.Header().Set("Content-Type", "application/problem+json")
		} else {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, respBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestSubcommandsHitCorrectEndpoint pins the routing + body contract for every admin subcommand:
// the CLI must call the exact E13 method/path with the resolved bearer and the right JSON body.
func TestSubcommandsHitCorrectEndpoint(t *testing.T) {
	cases := []struct {
		name                                 string
		args                                 []string
		stdin                                string
		wantMethod, wantPath, wantBodySubstr string
	}{
		{"org create", []string{"org", "create", "--display-name", "Acme"}, "", "POST", "/v1/organizations", `"display_name":"Acme"`},
		{"org list", []string{"org", "list"}, "", "GET", "/v1/organizations", ""},
		{"org get", []string{"org", "get", "org_1"}, "", "GET", "/v1/organizations/org_1", ""},
		{"project create", []string{"project", "create", "--display-name", "P"}, "", "POST", "/v1/projects", `"display_name":"P"`},
		{"project list", []string{"project", "list"}, "", "GET", "/v1/projects", ""},
		{"project get", []string{"project", "get", "prj_1"}, "", "GET", "/v1/projects/prj_1", ""},
		{"project set-policy", []string{"project", "set-policy", "prj_1", "--allowed-models", "m1,m2"}, "", "PATCH", "/v1/projects/prj_1", `"allowed_models":["m1","m2"]`},
		{"apikey create", []string{"apikey", "create", "--project", "prj_1", "--scope", "run"}, "", "POST", "/v1/api-keys", `"project_id":"prj_1"`},
		{"apikey list", []string{"apikey", "list"}, "", "GET", "/v1/api-keys", ""},
		{"apikey get", []string{"apikey", "get", "key_1"}, "", "GET", "/v1/api-keys/key_1", ""},
		{"apikey revoke", []string{"apikey", "revoke", "key_1"}, "", "POST", "/v1/api-keys/key_1/revoke", ""},
		{"secret create", []string{"secret", "create", "--name", "db-url"}, "postgres://s", "POST", "/v1/secret-refs", `"value":"postgres://s"`},
		{"secret list", []string{"secret", "list"}, "", "GET", "/v1/secret-refs", ""},
		{"secret get", []string{"secret", "get", "db-url"}, "", "GET", "/v1/secret-refs/db-url", ""},
		{"secret rotate", []string{"secret", "rotate", "db-url"}, "newval", "POST", "/v1/secret-refs/db-url/rotate", `"value":"newval"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			srv := stubServer(t, http.StatusOK, `{"object":"resource"}`, &cap)
			t.Setenv("PALAI_BASE_URL", srv.URL)
			t.Setenv("PALAI_API_KEY", "admin-key-xyz")

			var out bytes.Buffer
			if err := Run(tc.args[0], tc.args[1:], &out, strings.NewReader(tc.stdin)); err != nil {
				t.Fatalf("Run(%v): %v", tc.args, err)
			}
			if cap.method != tc.wantMethod {
				t.Errorf("method = %q, want %q", cap.method, tc.wantMethod)
			}
			if cap.path != tc.wantPath {
				t.Errorf("path = %q, want %q", cap.path, tc.wantPath)
			}
			if cap.auth != "Bearer admin-key-xyz" {
				t.Errorf("Authorization = %q, want %q", cap.auth, "Bearer admin-key-xyz")
			}
			if tc.wantBodySubstr != "" && !strings.Contains(cap.body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want to contain %q", cap.body, tc.wantBodySubstr)
			}
		})
	}
}

// TestSecretValueNeverInArgvOrOutput is the credential-hygiene contract for secrets: the value is
// read from stdin (there is NO --value flag, so it can never ride argv), it is sent in the body,
// and it never appears in the command's own output.
func TestSecretValueNeverInArgvOrOutput(t *testing.T) {
	var cap capture
	srv := stubServer(t, http.StatusCreated, `{"name":"db-url","object":"secret_ref","version":1}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")

	const secret = "super-secret-value"
	var out bytes.Buffer
	if err := Run("secret", []string{"create", "--name", "db-url"}, &out, strings.NewReader(secret)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(cap.body, secret) {
		t.Fatalf("secret value was not sent in the body: %q", cap.body)
	}
	if strings.Contains(out.String(), secret) {
		t.Fatalf("secret value leaked to stdout: %q", out.String())
	}
	if strings.Contains(out.String(), "admin-key-xyz") {
		t.Fatalf("admin key leaked to stdout: %q", out.String())
	}

	// A --value flag must not exist: passing one is a parse error, so a value can never be argv-borne.
	if err := Run("secret", []string{"create", "--name", "db-url", "--value", "x"}, io.Discard, strings.NewReader("")); err == nil {
		t.Fatal("expected an error for --value (no such flag — value must come from stdin)")
	}
}

// TestAPIKeyCreateShowsOneTimeKeyNotAdminKey proves the one-time disclosure rule: the create response's
// plaintext key IS printed once (the API's create-only field), while the admin key used to authenticate
// never appears in output.
func TestAPIKeyCreateShowsOneTimeKeyNotAdminKey(t *testing.T) {
	var cap capture
	srv := stubServer(t, http.StatusCreated, `{"id":"key_1","object":"api_key","key":"sk_newsecret"}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")

	var out bytes.Buffer
	if err := Run("apikey", []string{"create", "--project", "prj_1"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), "sk_newsecret") {
		t.Fatalf("the one-time key should be shown once: %q", out.String())
	}
	if strings.Contains(out.String(), "admin-key-xyz") {
		t.Fatalf("admin key leaked to stdout: %q", out.String())
	}
}

// TestProblemRender pins the RFC9457 rendering: the default mode renders a human line carrying the code
// and request id (as the returned error, which main prints), while --json writes the raw problem to stdout.
func TestProblemRender(t *testing.T) {
	const prob = `{"type":"https://docs.palai.dev/problems/insufficient_scope","title":"Insufficient scope","detail":"this API key lacks the provision capability","code":"insufficient_scope","request_id":"req_9","status":403}`

	var cap capture
	srv := stubServer(t, http.StatusForbidden, prob, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "k")

	var out bytes.Buffer
	err := Run("org", []string{"list"}, &out, strings.NewReader(""))
	if err == nil {
		t.Fatal("expected an error on 403")
	}
	if !strings.Contains(err.Error(), "insufficient_scope") || !strings.Contains(err.Error(), "req_9") {
		t.Fatalf("human render missing code/request id: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("human render must not write the body to stdout: %q", out.String())
	}

	var jout bytes.Buffer
	err = Run("org", []string{"list", "--json"}, &jout, strings.NewReader(""))
	if err == nil {
		t.Fatal("expected an error on 403 (json)")
	}
	if !strings.Contains(jout.String(), `"code":"insufficient_scope"`) {
		t.Fatalf("--json mode should print the raw problem to stdout: %q", jout.String())
	}
}

// TestResolveFallsBackToPalai proves the flag → env → .palai chain: with no flag and no env, both the base
// URL and the API key come from an initialised .palai stack.
func TestResolveFallsBackToPalai(t *testing.T) {
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "config.json"), []byte(`{"base_url":"http://from-palai:8080"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "api-key"), []byte("palai-key-from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PALAI_HOME", home)
	t.Setenv("PALAI_BASE_URL", "")
	t.Setenv("PALAI_API_KEY", "")

	baseURL, apiKey, err := resolve("", "")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if baseURL != "http://from-palai:8080" {
		t.Errorf("baseURL = %q, want the .palai config value", baseURL)
	}
	if apiKey != "palai-key-from-file" {
		t.Errorf("apiKey = %q, want the .palai api-key file value", apiKey)
	}
}

// TestFlagBeforePositional proves the interleaved parse: a flag placed before the positional id still
// parses, and the id still lands in the path (the stdlib flag package stops at the first non-flag, so this
// needs the interleaving helper to work at all).
func TestFlagBeforePositional(t *testing.T) {
	var cap capture
	srv := stubServer(t, http.StatusOK, `{}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "k")

	var out bytes.Buffer
	if err := Run("apikey", []string{"revoke", "--json", "key_42"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.path != "/v1/api-keys/key_42/revoke" {
		t.Fatalf("path = %q, want /v1/api-keys/key_42/revoke", cap.path)
	}
}

// TestAPIKeyFileFlagReadsKeyFromFile proves --api-key-file takes the key from a file (never argv) and it
// overrides env.
func TestAPIKeyFileFlagReadsKeyFromFile(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "key")
	if err := os.WriteFile(keyFile, []byte("file-borne-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var cap capture
	srv := stubServer(t, http.StatusOK, `{}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "env-key")

	var out bytes.Buffer
	if err := Run("org", []string{"list", "--api-key-file", keyFile}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.auth != "Bearer file-borne-key" {
		t.Fatalf("Authorization = %q, want the file-borne key (overriding env)", cap.auth)
	}
}
