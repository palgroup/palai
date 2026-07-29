package stack

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// moduleRoot walks up to the module root. It duplicates up_e2e_test.go's repoRoot under a different
// name on purpose: that one lives behind the e2e build tag, and these proofs must run untagged.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod above %s", dir)
		}
		dir = parent
	}
}

// TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell is the E21 T2 / §3.6 D5 RED, and the
// symptom it pins is the one the owner met as "secrets do not survive a restart".
//
// They always did. Secrets live in Postgres, envelope-encrypted, and identity/secrets.go re-reads the
// row on every resolve — there is no in-memory cache to lose. What was lost is the POINTER: this line
// interpolated PALAI_SECRET_MASTER_KEY_FILE from the shell that invoked compose, and the only thing
// that ever exported it was `palai up`'s applySlackEnv — which ran ONLY when .env.local held Slack
// credentials. So a plain `docker compose up -d`, or any `palai up` on a stack with no Slack app,
// handed the container an EMPTY value, left dbSecretStore nil, unmounted the secret-ref routes, and
// every handle resolved nowhere. deploy/compose/production.yml writes the path literally and has
// never had this bug; the local file must match it.
func TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(moduleRoot(t), "deploy", "compose", "compose.yaml"))
	if err != nil {
		t.Fatalf("read compose.yaml: %v", err)
	}
	line := ""
	for _, l := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "PALAI_SECRET_MASTER_KEY_FILE:") {
			line = strings.TrimSpace(l)
			break
		}
	}
	if line == "" {
		t.Fatal("compose.yaml no longer sets PALAI_SECRET_MASTER_KEY_FILE at all: the DB-backed secret store " +
			"would stay nil on every local stack and every *_ref handle would resolve nowhere")
	}
	if strings.Contains(line, "${") {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE is interpolated from the invoking shell (%q). "+
			"A `docker compose up -d` — or a `palai up` on a stack with no Slack credentials — then passes an EMPTY "+
			"value, dbSecretStore stays nil, and every secret handle resolves nowhere. Write the path literally, "+
			"as production.yml already does", line)
	}
	if !strings.Contains(line, containerMasterKeyPath) {
		t.Fatalf("PALAI_SECRET_MASTER_KEY_FILE = %q, want the compose file-secret mount %s", line, containerMasterKeyPath)
	}
}

// TestEveryBringUpLeavesABootableMasterKey is the other half of the same fix, and without it the line
// above is a way to BRICK the stack rather than repair it.
//
// The control-plane treats a SET-but-unparseable PALAI_SECRET_MASTER_KEY_FILE as log.Fatalf (main.go:
// "a broken key-file permission on redeploy must not boot healthy with the secret store silently
// disabled"). ensureSecretSlots used to create the master-key slot EMPTY — which was safe only while
// nothing named it. Now that compose names it unconditionally, every bring-up must leave a key the
// control-plane can parse, or `palai local up` boots into a crash loop.
//
// ensureSecretSlots is the right home because it is the ONE funnel every compose bring-up routes
// through: Up() calls it, and Bootstrap (`palai up`) calls Up().
func TestEveryBringUpLeavesABootableMasterKey(t *testing.T) {
	p := tempPaths(t)
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	key, err := readTrimmed(p.masterKey)
	if err != nil {
		t.Fatalf("read the master key slot: %v", err)
	}
	if len(key) != 64 || !isHex(key) {
		// The value is a KEY: report its shape, never its bytes.
		t.Fatalf("the master-key slot holds %d bytes of non-key material after a bring-up; compose now names this "+
			"file unconditionally, so the control-plane would log.Fatalf on it and the stack would never come up", len(key))
	}
	// A second bring-up must not re-mint: the key seals every secret already in the store, and dbSecret
	// fails CLOSED on a decrypt failure — re-minting would not degrade the stack, it would break it.
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	again, _ := readTrimmed(p.masterKey)
	if again != key {
		t.Fatal("a second bring-up replaced the master key: every secret already sealed under the old one is now dead")
	}
}

// TestMasterKeyIsNoLongerSlackConditional: the master key is not a Slack feature. applySlackEnv used to
// own it — which is exactly why a stack with no Slack app had no usable secret store. The bring-up must
// leave a bootable key whether or not any Slack value is present.
func TestMasterKeyIsNoLongerSlackConditional(t *testing.T) {
	p := tempPaths(t)
	if w := applySlackEnv(p, func(string) string { return "" }); len(w) != 0 {
		t.Fatalf("a Slack-free environment produced Slack warnings: %v", w)
	}
	if err := ensureSecretSlots(p); err != nil {
		t.Fatalf("ensure the secret slots: %v", err)
	}
	key, _ := readTrimmed(p.masterKey)
	if len(key) != 64 {
		t.Fatal("a bring-up with NO Slack credentials left no usable master key — the secret store would stay nil, " +
			"which is the whole of §3.6 D5")
	}
}

// --- the two ids `palai up` used to demand and silently skip over ---------------------------------

// TestRunTargetIsNoLongerAPrerequisiteForRegistration is the item-2 RED. Today a missing
// SLACK_AGENT_REVISION_ID / SLACK_PRINCIPAL_ID makes slackRegistration return a SKIP, `palai up` exits
// green, and the operator sees a successful install that cannot run anything. Registration must no
// longer refuse for want of ids the bring-up can provision itself.
func TestRunTargetIsNoLongerAPrerequisiteForRegistration(t *testing.T) {
	get := envGetter(map[string]string{
		"SLACK_TEAM_ID":        "T123",
		"SLACK_SIGNING_SECRET": "shhh",
	})
	body, skip := slackRegistration(get)
	if skip != "" {
		t.Fatalf("registration skipped for want of a run target it can provision: %q", skip)
	}
	if body["team_id"] != "T123" {
		t.Fatalf("team_id = %v", body["team_id"])
	}
	// The body is built WITHOUT a run target; wireSlack fills it from resolveRunTarget, so nothing here
	// may invent one.
	if _, present := body["default_policy"]; present {
		t.Fatal("slackRegistration invented a default_policy: the run target is resolved against the running stack, not guessed")
	}
}

// TestExplicitRunTargetAlwaysWins: an operator who names both ids gets exactly those, and the bring-up
// creates nothing. Explicit configuration is never overridden by provisioning.
func TestExplicitRunTargetAlwaysWins(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	target, err := resolveRunTarget(api, envGetter(map[string]string{
		"SLACK_AGENT_REVISION_ID": "arev_explicit",
		"SLACK_PRINCIPAL_ID":      "prin_explicit",
	}), false)
	if err != nil {
		t.Fatalf("resolve the run target: %v", err)
	}
	if target.revision != "arev_explicit" || target.principal != "prin_explicit" {
		t.Fatalf("explicit configuration was overridden: %+v", target)
	}
	if len(*calls) != 0 {
		t.Fatalf("the bring-up provisioned against a fully-configured run target: %v", *calls)
	}
	if target.resolved != "" {
		t.Fatalf("nothing was resolved, but the report claims %q", target.resolved)
	}
}

// TestMissingRunTargetIsProvisionedAndSaidOutLoud is the behaviour the plan names: with neither id set,
// `palai up` establishes both over the EXISTING provisioning API (no new path), and PRINTS what it
// bound. A binding the operator is not told about is the same silence in a new place.
func TestMissingRunTargetIsProvisionedAndSaidOutLoud(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	target, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("resolve the run target: %v", err)
	}
	if target.revision == "" || target.principal == "" {
		t.Fatalf("the run target was not provisioned: %+v", target)
	}
	if target.resolved == "" {
		t.Fatal("both ids were established and the operator is told nothing — the silent skip has become a silent create")
	}
	if !strings.Contains(target.resolved, target.revision) || !strings.Contains(target.resolved, target.principal) {
		t.Fatalf("the report line names neither id it bound: %q", target.resolved)
	}
	// The revision must be PUBLISHED: coordinator.store verifies published_at before pinning a run, so a
	// draft revision is a registration that admits nothing — today's failure with an extra step.
	if !strings.Contains(strings.Join(*calls, " "), "/publish") {
		t.Fatalf("the created agent revision was never published, so no run can pin it: %v", *calls)
	}
	// It opens NO new path: every call is an existing /v1 provisioning route.
	for _, c := range *calls {
		if !strings.Contains(c, "/v1/agents") && !strings.Contains(c, "/v1/api-keys") {
			t.Fatalf("the bring-up called %q — the plan allows only the EXISTING provisioning API", c)
		}
	}
}

// TestProvisionedRunTargetIsReusedOnASecondBringUp: `palai up` is re-run constantly. A bring-up that
// minted a fresh agent profile every time would leave a pile of orphan revisions and change which one
// the workspace is bound to on each run.
func TestProvisionedRunTargetIsReusedOnASecondBringUp(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	first, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := len(*calls)
	second, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision != second.revision {
		t.Fatalf("a second bring-up minted a second agent revision (%s then %s): the binding would move under the operator",
			first.revision, second.revision)
	}
	for _, c := range (*calls)[before:] {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("the second bring-up wrote %q against an already-provisioned run target", c)
		}
	}
	// It still SAYS what it bound — an operator re-running `palai up` needs the ids as much as the
	// one who ran it first — but it says "reused", not "created".
	if !strings.Contains(second.resolved, "reused") {
		t.Fatalf("the second bring-up reports its reused revision as though it made it: %q", second.resolved)
	}
}

// TestSkipIsReportedAsAWarningNotSwallowed is item 3. A half-configured Slack install that does nothing
// must not read as a clean success. The no-team case stays a plain line: an operator with no Slack app
// did not ask for one, and warning them every bring-up is the same crying-wolf this task removes from
// the orphan sweep.
func TestSkipIsReportedAsAWarningNotSwallowed(t *testing.T) {
	half := slackSkipWarning(envGetter(map[string]string{"SLACK_TEAM_ID": "T123"}), "SLACK_SIGNING_SECRET is missing")
	if half == "" {
		t.Fatal("a configured-but-unusable Slack install skipped silently inside a green bring-up")
	}
	if !strings.Contains(half, "SLACK_SIGNING_SECRET is missing") {
		t.Fatalf("the warning does not carry the reason: %q", half)
	}
	if none := slackSkipWarning(envGetter(nil), "no SLACK_TEAM_ID in the environment"); none != "" {
		t.Fatalf("an operator with no Slack app at all was warned about it: %q", none)
	}
}

// --- helpers ---------------------------------------------------------------------------------------

func envGetter(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

// fakeProvisioningAPI stands in for a running control-plane's provisioning surface: the bootstrap key's
// principal, the agent-profile lineage, its revisions, and publish. It records every path so a test can
// assert the bring-up opened NO new route, and it keeps created state so a second resolve can reuse.
func fakeProvisioningAPI(t *testing.T) (*apiClient, *[]string) {
	c, calls, _ := fakeProvisioningAPIWithTools(t)
	return c, calls
}

// fakeProvisioningAPIWithTools additionally hands back the switch for the revision list's `tools` field.
func fakeProvisioningAPIWithTools(t *testing.T) (*apiClient, *[]string, *bool) {
	t.Helper()
	var calls []string
	profileID, revisionID, published := "", "", false
	// The revision remembers the tools it was created with. A fake that forgets them is not modelling this
	// API: `palai up` reuses a published revision only when its tool list already matches what was asked
	// for, so a forgetful fixture would prove reuse that the real server never performs.
	var revisionTools, revisionMCP []string
	revisionSeq := 0
	// listCarriesTools models the server-side CONFIG fields this fixture depends on — `tools` since E21 T4
	// and `mcp_connections` since E22 T6. A test can turn it OFF to reproduce a server that omits them and
	// prove the reuse check actually needs them.
	listCarriesTools := true
	// The repository-binding surface (E22 T3), modelled the way the real one behaves: a re-POST registers a
	// DISTINCT binding (bindings are durable configuration, not idempotent operations — api/repository_bindings.go
	// says so), so a bring-up that does not look before it creates will visibly pile them up here.
	var bindings []map[string]any
	bindingSeq := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		write := func(code int, v any) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(v)
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/api-keys/"+bootstrapKeyID:
			write(http.StatusOK, map[string]any{"id": bootstrapKeyID, "principal_id": "prin_local"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents":
			data := []any{}
			if profileID != "" {
				data = append(data, map[string]any{"id": profileID, "name": slackAgentProfileName})
			}
			write(http.StatusOK, map[string]any{"object": "list", "data": data})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents":
			profileID = "aprof_fake"
			write(http.StatusCreated, map[string]any{"id": profileID, "name": slackAgentProfileName})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/agents/"+profileID+"/revisions":
			data := []any{}
			if revisionID != "" && published {
				// The revision list mirrors what the REAL server returns. This was WRONG until 2026-07-27:
				// the fake carried `tools` here while apps/control-plane/internal/store/agents.go's
				// ListAgentRevisions did not serialise it, so every reuse check compared against nil and
				// `palai up` minted a fresh revision on EVERY bring-up. Measured against the running stack,
				// not reasoned about — two published revisions with identical tool lists.
				rev := map[string]any{"id": revisionID, "status": "published"}
				if listCarriesTools {
					rev["tools"] = revisionTools
					rev["mcp_connections"] = revisionMCP
				}
				data = append(data, rev)
			}
			write(http.StatusOK, map[string]any{"object": "list", "data": data})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/"+profileID+"/revisions":
			var body struct {
				Tools          []string `json:"tools"`
				MCPConnections []string `json:"mcp_connections"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			revisionTools, revisionMCP = body.Tools, body.MCPConnections
			// A DISTINCT id per revision, as the real server issues: a fixture that reuses one id cannot
			// show the difference between reusing a revision and minting a replacement.
			revisionSeq++
			revisionID = fmt.Sprintf("arev_fake_%d", revisionSeq)
			published = false
			write(http.StatusCreated, map[string]any{"id": revisionID, "status": "draft"})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/agents/"+profileID+"/revisions/"+revisionID+"/publish":
			published = true
			write(http.StatusOK, map[string]any{"id": revisionID, "status": "published"})
		case r.Method == http.MethodGet && r.URL.Path == "/v1/repository-bindings":
			write(http.StatusOK, map[string]any{"object": "list", "data": bindings})
		case r.Method == http.MethodPost && r.URL.Path == "/v1/repository-bindings":
			var body struct {
				Provider           string `json:"provider"`
				RepositoryIdentity string `json:"repository_identity"`
				CloneURL           string `json:"clone_url"`
				DefaultBranch      string `json:"default_branch"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			// The real handler's own refusal (provider, repository_identity and clone_url are required), so a
			// bring-up that derives an empty identity fails HERE rather than passing against a lenient fake.
			if body.Provider == "" || body.RepositoryIdentity == "" || body.CloneURL == "" {
				write(http.StatusBadRequest, map[string]any{"detail": "provider, repository_identity, and clone_url are required"})
				return
			}
			bindingSeq++
			binding := map[string]any{
				"id": fmt.Sprintf("repo_fake_%d", bindingSeq), "object": "repository_binding",
				"provider": body.Provider, "repository_identity": body.RepositoryIdentity,
				"clone_url": body.CloneURL, "default_branch": body.DefaultBranch,
			}
			bindings = append(bindings, binding)
			write(http.StatusCreated, binding)
		default:
			write(http.StatusNotFound, map[string]any{"detail": "no such route " + r.URL.Path})
		}
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}, &calls, &listCarriesTools
}
