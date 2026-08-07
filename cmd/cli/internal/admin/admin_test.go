package admin

import (
	"bytes"
	"flag"
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
		// --pool is E24's central knob and had no flag until T6 measured it while writing the operator page:
		// `config_policy.pool` shipped on the endpoint (T2) and could only be set with a raw PATCH.
		{"apikey create", []string{"apikey", "create", "--project", "prj_1", "--scope", "run"}, "", "POST", "/v1/api-keys", `"project_id":"prj_1"`},
		{"apikey list", []string{"apikey", "list"}, "", "GET", "/v1/api-keys", ""},
		{"apikey get", []string{"apikey", "get", "key_1"}, "", "GET", "/v1/api-keys/key_1", ""},
		{"apikey revoke", []string{"apikey", "revoke", "key_1"}, "", "POST", "/v1/api-keys/key_1/revoke", ""},
		{"secret create", []string{"secret", "create", "--name", "db-url"}, "postgres://s", "POST", "/v1/secret-refs", `"value":"postgres://s"`},
		{"secret list", []string{"secret", "list"}, "", "GET", "/v1/secret-refs", ""},
		{"secret get", []string{"secret", "get", "db-url"}, "", "GET", "/v1/secret-refs/db-url", ""},
		{"secret rotate", []string{"secret", "rotate", "db-url"}, "newval", "POST", "/v1/secret-refs/db-url/rotate", `"value":"newval"`},
		// The runner-pool enrolment key (E24 T3). Note what is NOT in any of these three arg lists: a key
		// value. `create` returns one, `list` never sees one, and `revoke` names the key by id.
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
	err := Run("pool", []string{"list"}, &out, strings.NewReader(""))
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
	err = Run("pool", []string{"list", "--json"}, &jout, strings.NewReader(""))
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
	if err := Run("pool", []string{"list", "--api-key-file", keyFile}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if cap.auth != "Bearer file-borne-key" {
		t.Fatalf("Authorization = %q, want the file-borne key (overriding env)", cap.auth)
	}
}

// ‼️ TestPoolKeyCreateRequiresAPoolAndPrintsTheValueOnce LEFT WITH THE `poolkey` VERB (2026-08-07), and
// its two claims went to two different places rather than being dropped:
//
//   - "the one-time value is shown exactly once" is the PRODUCT's claim and it is asserted where the value
//     is minted: runner_pool_key_component_test.go — "the value is shown exactly once and this was that
//     once", against a real database.
//   - "a create with no --pool is refused before any request" was a CLI-side guard against minting a
//     credential for a fleet nobody named. It becomes STRUCTURAL without the CLI: the route is
//     POST /v1/runner-pools/{pool_id}/keys, so the pool is in the path and there is nothing to default.
//
// A claim that moved is not a claim that was lost, and a claim that a URL makes impossible is stronger
// than one a flag parser enforced.
func TestPoolKeyRevokeRejectsASecondPositional(t *testing.T) {
	var cap capture
	srv := stubServer(t, http.StatusOK, `{}`, &cap)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")
	err := Run("poolkey", []string{"revoke", "rpk_1", "rpk_secretvaluepasted"}, io.Discard, strings.NewReader(""))
	if err == nil {
		t.Fatal("a second positional was accepted")
	}
	if strings.Contains(err.Error(), "rpk_secretvaluepasted") {
		t.Fatalf("the error echoed the extra positional, which may be a credential: %v", err)
	}
	if cap.method != "" {
		t.Fatalf("the rejected command still issued %s %s", cap.method, cap.path)
	}
}

// THE CLI VERB THE CONSOLE CLAIMED EXISTED (E29).
//
// apps/web-console/lib/routes.ts told every operator, in the lead of the model-wiring screen: "This is a
// read surface — every row here is created by the API or the CLI." The API half was true. THE CLI HALF WAS
// NOT: `grep -rn 'model-connections' cmd/ | grep -v _test` returned nothing, so an operator who went looking
// for the verb the console named found no verb at all. That sentence is now corrected AND made true, and
// this table is the "made true" half.
//
// NOTE WHAT IS NOT IN ANY OF THESE ARG LISTS: a credential. A provider key is written with
// `palai secret create` (stdin), and a connection then names the REF. That split is the whole write-only
// discipline, and preserving it here is why there is no `--api-key` flag to add.
func TestModelConnectionSubcommandsHitCorrectEndpoint(t *testing.T) {
	cases := []struct {
		name                                 string
		args                                 []string
		wantMethod, wantPath, wantBodySubstr string
	}{
		{"model create", []string{"model", "create", "--provider", "provider-one", "--secret-ref", "openai"},
			"POST", "/v1/model-connections", `"provider":"provider-one"`},
		// The third shape the product promises: an operator's own OpenAI-compatible endpoint.
		{"model create custom endpoint", []string{"model", "create", "--provider", "openai-compatible", "--secret-ref", "vllm", "--endpoint", "http://10.0.0.11:8000/v1/chat/completions"},
			"POST", "/v1/model-connections", `"base_url":"http://10.0.0.11:8000/v1/chat/completions"`},
		{"model list", []string{"model", "list"}, "GET", "/v1/model-connections", ""},
		{"model get", []string{"model", "get", "mconn_1"}, "GET", "/v1/model-connections/mconn_1", ""},
		{"model verify", []string{"model", "verify", "mconn_1"}, "POST", "/v1/model-connections/mconn_1/verify", ""},
		// The models list (E29 provider models): the provider's OWN catalogue for this connection's
		// credential. A GET, unlike the verify above, because it writes no stamp.
		{"model models", []string{"model", "models", "mconn_1"}, "GET", "/v1/model-connections/mconn_1/models", ""},
		{"model routes", []string{"model", "routes"}, "GET", "/v1/model-routes", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			srv := stubServer(t, http.StatusOK, `{"object":"resource","id":"mroute_1"}`, &cap)
			t.Setenv("PALAI_BASE_URL", srv.URL)
			t.Setenv("PALAI_API_KEY", "admin-key-xyz")

			var out bytes.Buffer
			if err := Run(tc.args[0], tc.args[1:], &out, strings.NewReader("")); err != nil {
				t.Fatalf("Run(%v): %v", tc.args, err)
			}
			if cap.method != tc.wantMethod || cap.path != tc.wantPath {
				t.Errorf("%s %s, want %s %s", cap.method, cap.path, tc.wantMethod, tc.wantPath)
			}
			if tc.wantBodySubstr != "" && !strings.Contains(cap.body, tc.wantBodySubstr) {
				t.Errorf("body = %q, want to contain %q", cap.body, tc.wantBodySubstr)
			}
		})
	}
}

// NO FLAG ON THIS RESOURCE MAY CARRY A CREDENTIAL. The `model` verb registers flags for a provider family,
// a secret REF NAME and a URL — every one of them a public fact — and the guard is written as a test rather
// than a comment because the tempting next flag is `--api-key`, which would put a provider credential in
// argv, in the shell history and in every `ps` on the box.
func TestModelFlagsCarryNoCredential(t *testing.T) {
	var f flags
	fs := flag.NewFlagSet("model", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	f.register(fs, "model")

	fs.VisitAll(func(fl *flag.Flag) {
		for _, banned := range []string{"key", "secret-value", "value", "token", "password", "credential"} {
			if fl.Name == banned {
				t.Errorf("`palai model` registers --%s: a provider credential must never reach argv — "+
					"write it with `palai secret create` (stdin) and name the REF here", fl.Name)
			}
		}
	})
}

// `palai model route` IS THE HALF WITHOUT WHICH A CONNECTION IS A ROW NOTHING READS (E29).
//
// A run does not resolve a connection. It resolves an ALIAS, through a PUBLISHED revision, to a connection —
// so an operator who created a connection and stopped has changed nothing observable about their deployment.
// The three calls are asserted IN ORDER, because the order is the contract: a revision cannot exist before
// its route, and a DRAFT revision steers nothing until the publish lands. Asserting only the last call would
// pass on a command that published a revision of a route it never opened.
func TestModelRoutePublishesTheRevisionItCreates(t *testing.T) {
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/model-routes":
			_, _ = io.WriteString(w, `{"id":"mroute_9","object":"model_route"}`)
		case strings.HasSuffix(r.URL.Path, "/revisions"):
			_, _ = io.WriteString(w, `{"id":"mrev_9","object":"model_route_revision"}`)
		default:
			_, _ = io.WriteString(w, `{"id":"mrev_9","published":true}`)
		}
	}))
	defer srv.Close()
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")

	var out bytes.Buffer
	if err := Run("model", []string{"route", "--connection", "mconn_1", "--model", "gpt-4o-mini"}, &out, strings.NewReader("")); err != nil {
		t.Fatalf("model route: %v", err)
	}
	want := []string{
		"POST /v1/model-routes",
		"POST /v1/model-routes/mroute_9/revisions",
		"POST /v1/model-routes/mroute_9/revisions/mrev_9/publish",
	}
	if len(seen) != len(want) {
		t.Fatalf("calls = %v, want %v", seen, want)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Fatalf("call %d = %q, want %q (the ids must be the ones THIS command minted, not a reused one)", i, seen[i], want[i])
		}
	}
}

// A create that answers without an `id` stops the command instead of threading an empty string into the
// next path. `POST /v1/model-routes//revisions` would 404, and the operator would go looking for a route
// that was never the problem.
func TestModelRouteRefusesAnIdlessCreate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"model_route"}`) // no id
	}))
	defer srv.Close()
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key-xyz")

	var out bytes.Buffer
	err := Run("model", []string{"route", "--connection", "mconn_1", "--model", "gpt-4o-mini"}, &out, strings.NewReader(""))
	if err == nil || !strings.Contains(err.Error(), "no `id`") {
		t.Fatalf("err = %v, want a refusal naming the missing id", err)
	}
}
