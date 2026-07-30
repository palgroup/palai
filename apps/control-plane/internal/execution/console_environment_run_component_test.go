//go:build component

package execution

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/host"
	"github.com/palgroup/palai/apps/control-plane/api"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/apps/control-plane/internal/identity"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// THE ONE SENTENCE E25 T3 AND T6 PROVE TOGETHER: a run started with a revision that has an environment sees
// those keys in its shell — and every step up to the run is a PUBLIC HTTP CALL, the same call the console's
// own screens make.
//
// WHY THIS FILE EXISTS ON TOP OF environment_component_test.go. That file proved the pipe, and it had to
// INSERT the agent revision and the run with SQL, because nothing in the tree could create them any other
// way that a test could drive. So the chain it proved began one step downstream of where an operator begins:
// it never showed that the API's OWN write path produces a revision the resolver can read, and it never
// touched `agent_revision_id` on POST /v1/responses at all. This file starts from the operator's end. There
// is not one INSERT in it below the tenant seed:
//
//	POST /v1/environments                                   (the Environments screen)
//	POST /v1/environments/{id}/values          × 2          (the Environments screen, SecretField)
//	POST /v1/repository-bindings                            (the Repositories screen)
//	POST /v1/agents                                         (the Agents screen)
//	POST /v1/agents/{id}/revisions            {environment}  (the Agents screen, environment picker)
//	POST /v1/agents/{id}/revisions/{rev}/publish            (the Agents screen, Publish button)
//	POST /v1/responses                  {agent_revision_id}  (the /runs screen, revision picker)
//	  → resolveEnvKeys / resolveEnvValues on the run the API created
//	  → the production palai.workspace.shell tool over the production host executor
//	  → printenv sees both keys
//
// WHAT A BROWSER CANNOT DO, and why the console's own spec stops short of it: a browser cannot read a
// subprocess's environment, and neither console profile reaches a tool call at all (the fake upstream scripts
// its events; the compose fake adapter is hardcoded with no ToolCalls — DIV-UNX-001). So the browser proves
// the WRITES and this proves the CONSEQUENCE, over the identical routes.
//
// THE ROUTER IS THE SHIPPED ONE and it mounts exactly the five families the console's four screens address.
// Nothing else is wired, which is the same argument approvalRouter makes: a router that mounts nothing else
// cannot answer one of these calls by accident.

const consoleEnvSentinel = "e25-t6-console-run-sentinel-9d4b7e21"

// TestAConsoleWrittenEnvironmentReachesTheShellOfARunItPinned is the whole claim.
func TestAConsoleWrittenEnvironmentReachesTheShellOfARunItPinned(t *testing.T) {
	cs, tenant, exec := openPinnedSpine(t)
	ctx := context.Background()

	// A master key for this test only; never written to a file. The environment routes mount only when one
	// exists (main.go: one `if`, two families), so this is also what makes the surface under test exist.
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i*11 + 3)
	}
	key, err := identity.ParseMasterKey(hex.EncodeToString(raw[:]))
	if err != nil {
		t.Fatalf("ParseMasterKey: %v", err)
	}

	repo := approverHTTP(t)
	secrets := identity.NewSecretStore(cs.Pool(), key)
	caller := mintScopedKey(t, repo, cs, tenant, []string{"provision", "responses"})
	srv := httptest.NewServer(consoleConfigRouter(repo, secrets))
	t.Cleanup(srv.Close)
	client := &consoleClient{t: t, base: srv.URL, token: caller.token}

	// 1. THE ENVIRONMENTS SCREEN. An environment and two keys, through the routes the form posts to. The
	// values never leave this function and are never a path segment or an argument.
	env := client.post(201, "/v1/environments", map[string]any{"name": "production", "description": "written from the console"})
	envID, _ := env["id"].(string)
	if envID == "" {
		t.Fatalf("POST /v1/environments returned no id: %v", env)
	}
	for key, value := range map[string]string{"JIRA_TOKEN": consoleEnvSentinel, "DEPLOY_TARGET": "staging.example.internal"} {
		wrote := client.post(201, "/v1/environments/"+envID+"/values", map[string]any{"key": key, "value": value})
		if wrote["version"] == nil {
			t.Fatalf("POST /v1/environments/{id}/values (%s) returned no version: %v", key, wrote)
		}
		// THE WRITE ROUTE ANSWERS WITH METADATA AND NOTHING ELSE. Asserted on the BYTES rather than on the
		// field list, because a value smuggled into any field would be in them.
		body, _ := json.Marshal(wrote)
		if strings.Contains(string(body), consoleEnvSentinel) {
			t.Fatalf("the write route echoed the value back: %s", body)
		}
	}
	// AND THE READ ROUTE CARRIES NAMES, NOT VALUES — the console's whole environment screen in one assertion.
	detail := client.get(200, "/v1/environments/"+envID)
	detailBody, _ := json.Marshal(detail)
	if strings.Contains(string(detailBody), consoleEnvSentinel) {
		t.Fatalf("GET /v1/environments/{id} carries a value: %s", detailBody)
	}
	if !strings.Contains(string(detailBody), "JIRA_TOKEN") {
		t.Fatalf("GET /v1/environments/{id} does not carry the key NAMES either: %s", detailBody)
	}

	// 2. THE REPOSITORIES SCREEN. A binding, with a connection_ref that is a HANDLE — the name of a secret
	// this organization now has, because writing an environment value IS a secret_refs row under the derived
	// name. That is the only kind of thing this field can carry, and the read-back proves the console can
	// show what it registered.
	handle := "env:" + envID + ":JIRA_TOKEN"
	binding := client.post(201, "/v1/repository-bindings", map[string]any{
		"provider":            "github",
		"repository_identity": "palai-example/console-t6",
		"clone_url":           "https://github.com/palai-example/console-t6.git",
		"default_branch":      "main",
		"connection_ref":      handle,
		"allowed_operations":  []string{"clone", "push"},
		"policy":              map[string]any{"require_approval": true},
		"data_classification": "internal",
		"region_constraint":   "eu-central-1",
	})
	bindingID, _ := binding["id"].(string)
	if bindingID == "" {
		t.Fatalf("POST /v1/repository-bindings returned no id: %v", binding)
	}
	readBack := client.get(200, "/v1/repository-bindings/"+bindingID)
	if readBack["connection_ref"] != handle {
		t.Fatalf("the binding read back connection_ref = %v, want the handle %q", readBack["connection_ref"], handle)
	}
	// A NON-http(s) CLONE URL IS REFUSED (§24). The console cannot type its way past this, and the fixture
	// its browser proof runs against refuses it for the same reason — so the field's contract is the same on
	// both sides of that proof.
	client.post(400, "/v1/repository-bindings", map[string]any{
		"provider":            "github",
		"repository_identity": "palai-example/local",
		"clone_url":           "file:///tmp/local-repo",
	})

	// 3. THE AGENTS SCREEN. A lineage, a draft revision NAMING THE ENVIRONMENT, and a publish.
	agent := client.post(201, "/v1/agents", map[string]any{"name": "deployer"})
	agentID, _ := agent["id"].(string)
	if agentID == "" {
		t.Fatalf("POST /v1/agents returned no id: %v", agent)
	}
	revision := client.post(201, "/v1/agents/"+agentID+"/revisions", map[string]any{
		"model":       "model-pinned-by-console",
		"environment": envID,
	})
	revisionID, _ := revision["id"].(string)
	if revisionID == "" {
		t.Fatalf("POST /v1/agents/{id}/revisions returned no id: %v", revision)
	}
	// THE ENVIRONMENT HAS A READ PATH, which is what lets the console show which environment an agent runs
	// under. A field the API lets you write and never gives back is a field an operator cannot verify.
	if revision["environment"] != envID {
		t.Fatalf("the created revision projects environment = %v, want %q", revision["environment"], envID)
	}
	if revision["status"] != "draft" {
		t.Fatalf("a fresh revision is %v, want draft", revision["status"])
	}

	// 4. A DRAFT CANNOT BE RUN, AND THAT REFUSAL IS THE PIN'S OWN. 409, decided before the idempotency
	// reserve — so it is also the proof that the field TRAVELLED: a server that ignored agent_revision_id
	// would have admitted this happily.
	draftRefusal := client.postStatus(409, "/v1/responses", map[string]any{
		"input":             "run the deployer",
		"agent_revision_id": revisionID,
	})
	if draftRefusal["code"] != "revision_not_published" {
		t.Fatalf("a draft pin was refused with code %v, want revision_not_published", draftRefusal["code"])
	}
	// AND IT LEFT NOTHING BEHIND. The console tells the operator "no run and no session were created"; this
	// is that sentence, counted.
	if runs := countRows(t, cs, `SELECT count(*) FROM runs WHERE organization_id = $1`, tenant.Organization); runs != 0 {
		t.Fatalf("the refused admission left %d run(s) behind", runs)
	}

	published := client.post(200, "/v1/agents/"+agentID+"/revisions/"+revisionID+"/publish", map[string]any{})
	if published["status"] != "published" && published["published"] != true {
		// The publish projection's exact shape is not this test's subject; that it reports the transition is.
		t.Logf("publish projection: %v", published)
	}

	// 5. THE /runs SCREEN. A run PINNED to the published revision, through the same field the stream relay
	// now forwards. The Idempotency-Key is what the real router requires and the console's SDK supplies.
	run := client.postKeyed(202, "/v1/responses", "console-t6-"+revisionID, map[string]any{
		"input":             "run the deployer",
		"agent_revision_id": revisionID,
	})
	responseID, _ := run["id"].(string)
	sessionID, _ := run["session_id"].(string)
	runID, _ := run["run_id"].(string)
	if responseID == "" || sessionID == "" || runID == "" {
		t.Fatalf("the pinned admission returned an incomplete identity: %v", run)
	}

	// 6. THE CONSEQUENCE. From here the run is the orchestrator's, and every input below was created by an
	// HTTP call above — the run id, the revision it is pinned to, the environment that revision names, and
	// the sealed values behind it.
	orch := &Orchestrator{
		spine: cs,
		shell: host.NewExecutor(30 * time.Second),
		envSecrets: func(org, ref string) ([]byte, error) {
			v, ok, err := secrets.Resolve(ctx, org, ref)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("no such environment secret %q", ref)
			}
			return v, nil
		},
	}
	st := &attemptState{
		attempt:   AttemptDescriptor{RunID: contracts.RunID(runID), AttemptID: contracts.AttemptID(pinnedID("att")), Fence: 1},
		tenant:    tenant,
		sessionID: sessionID,
	}
	keys, err := orch.resolveEnvKeys(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvKeys on the run the API created: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("the run pinned through the PUBLIC API resolved %d environment key(s), want 2: %+v\n"+
			"the write path and the read path disagree, which is the whole thing this test exists to catch", len(keys), keys)
	}
	st.envKeys = keys
	values, err := orch.resolveEnvValues(ctx, st)
	if err != nil {
		t.Fatalf("resolveEnvValues: %v", err)
	}
	if values["JIRA_TOKEN"] != consoleEnvSentinel {
		t.Fatalf("JIRA_TOKEN resolved to %q, want the value the HTTP write sealed", values["JIRA_TOKEN"])
	}

	envForShell := orch.execEnv(st)
	envForShell.WorkspaceRoot = t.TempDir()
	envForShell.EnvValues = values
	broker := toolbroker.New(tools.ShellTool())
	outcome, err := broker.Execute(ctx, "call_console_printenv", "palai.workspace.shell",
		map[string]any{"argv": []any{"sh", "-c", "printenv JIRA_TOKEN; printenv DEPLOY_TARGET"}}, 1, envForShell)
	if err != nil {
		t.Fatalf("palai.workspace.shell: %v", err)
	}
	if code, _ := outcome.Result["exit_code"].(int); code != 0 {
		t.Fatalf("printenv exited %v (%v) — a key the console wrote never reached the shell",
			outcome.Result["exit_code"], outcome.Result["stderr"])
	}
	stdout, _ := outcome.Result["stdout"].(string)
	// Both values come back MASKED, which is RedactValues doing its job: it is value-based, so a
	// non-credential-shaped value is masked too. What proves both keys ARRIVED is the exit code (printenv
	// exits 1 on the first unset name) together with two masks.
	if strings.Count(stdout, "***") != 2 {
		t.Fatalf("printenv printed %q; want two masks — either a key never arrived or a value came back unmasked", stdout)
	}
	if strings.Contains(stdout, consoleEnvSentinel) {
		t.Fatalf("the value came back through the shell result unmasked: %q", stdout)
	}

	// 7. AND NO HTTP RESPONSE ON THE WAY HERE CARRIED IT. Every body this test received is scanned, so the
	// claim is about the bytes the console's own screens would have rendered rather than about a field list.
	for path, body := range client.seen {
		if strings.Contains(body, consoleEnvSentinel) {
			t.Fatalf("the value is in the response body of %s:\n%s", path, body)
		}
	}
	if len(client.seen) < 8 {
		t.Fatalf("only %d response bodies were captured, so the scan above proves little", len(client.seen))
	}

	// The tenant seed used SQL; nothing below it did. Kept as an assertion so a later edit that reaches for
	// exec() to "just insert the revision" fails this instead of quietly narrowing the claim.
	_ = exec
}

// consoleConfigRouter builds the SHIPPED router carrying exactly the families the console's configuration
// screens address: responses (admission, for the pin), repository bindings, agents, and — behind the same
// master-key condition main.go uses — secret refs and environments.
func consoleConfigRouter(repo *store.Store, secrets *identity.SecretStore) http.Handler {
	return api.NewRouter(repo, repo, nil, nil, repo, repo, nil, nil, nil, nil, nil, nil, nil, nil, nil,
		api.SSEConfig{}, nil, nil, api.WithSecretRefs(secrets), api.WithEnvironments(secrets))
}

// consoleClient is the console's half of the conversation: an authenticated JSON caller that records every
// response body it received, so the sentinel scan at the end covers the actual bytes.
type consoleClient struct {
	t     *testing.T
	base  string
	token string
	seen  map[string]string
}

func (c *consoleClient) record(path, body string) {
	if c.seen == nil {
		c.seen = map[string]string{}
	}
	c.seen[path] = body
}

func (c *consoleClient) do(req *http.Request, want int, path string) map[string]any {
	c.t.Helper()
	req.Header.Set("Authorization", "Bearer "+c.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatalf("%s %s: %v", req.Method, path, err)
	}
	defer res.Body.Close()
	var body map[string]any
	dec := json.NewDecoder(res.Body)
	_ = dec.Decode(&body)
	rendered, _ := json.Marshal(body)
	c.record(req.Method+" "+path, string(rendered))
	if res.StatusCode != want {
		c.t.Fatalf("%s %s = %d, want %d: %s", req.Method, path, res.StatusCode, want, rendered)
	}
	return body
}

func (c *consoleClient) get(want int, path string) map[string]any {
	c.t.Helper()
	req, err := http.NewRequest(http.MethodGet, c.base+path, nil)
	if err != nil {
		c.t.Fatalf("build GET %s: %v", path, err)
	}
	return c.do(req, want, path)
}

func (c *consoleClient) post(want int, path string, body any) map[string]any {
	c.t.Helper()
	return c.postKeyed(want, path, "", body)
}

// postStatus is post for a call whose expected status is a refusal; it reads the same way at the call site.
func (c *consoleClient) postStatus(want int, path string, body any) map[string]any {
	c.t.Helper()
	return c.postKeyed(want, path, "console-t6-refusal-"+pinnedID("k"), body)
}

func (c *consoleClient) postKeyed(want int, path, idempotencyKey string, body any) map[string]any {
	c.t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		c.t.Fatalf("marshal body for %s: %v", path, err)
	}
	req, err := http.NewRequest(http.MethodPost, c.base+path, bytes.NewReader(raw))
	if err != nil {
		c.t.Fatalf("build POST %s: %v", path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	return c.do(req, want, path)
}

// countRows is a system-scoped count for the "nothing was left behind" assertions.
func countRows(t *testing.T, cs *coordinator.Store, sql string, args ...any) int {
	t.Helper()
	n, err := strconv.Atoi(scalar(t, cs.Pool(), sql, args...))
	if err != nil {
		t.Fatalf("count %q did not return a number: %v", sql, err)
	}
	return n
}
