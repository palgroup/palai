package stack

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestBringUpGrantsTheCanonicalDefaultSet pins the ONE fact this task exists for: the names the CLI
// writes into prj_local's policy are the canonical list, not a copy of it. A copy is what commit
// 1e5fc63e deleted and what left the guard hunting a symbol that no longer existed.
func TestBringUpGrantsTheCanonicalDefaultSet(t *testing.T) {
	granted := bootstrapDefaultTools()
	want := toolset.Default()
	if !reflect.DeepEqual(granted, want) {
		t.Fatalf("bring-up grants %v, canonical Default() is %v", granted, want)
	}
}

// TestTheGrantPreservesEveryOtherPolicyKey is the guard against the trap named in Step 1: the policy
// write REPLACES the document. A bring-up that sent only default_tools would delete approvers, pool,
// and allowed_models on its SECOND run — and approvers is an allow-list an operator deliberately
// closed. The first run is not where this bites, which is exactly why it needs a test.
func TestTheGrantPreservesEveryOtherPolicyKey(t *testing.T) {
	existing := map[string]any{
		"approvers":      []string{"key:apik_1"},
		"pool":           "mac-mini",
		"allowed_models": []string{"m1"},
	}
	merged := policyWithDefaultTools(existing, toolset.Default())

	for key, want := range existing {
		got, present := merged[key]
		if !present {
			t.Errorf("the write dropped %q: a re-run would clear it from the live policy", key)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the write changed %q: got %v, want %v", key, got, want)
		}
	}
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Errorf("default_tools = %v, want the canonical set", merged["default_tools"])
	}
}

// TestTheGrantDoesNotMutateTheCallersPolicy — the caller holds the document it just read from the
// server; a merge that wrote through would make the "before" and "after" the same object and hide a
// diff the caller may want to log or skip on.
func TestTheGrantDoesNotMutateTheCallersPolicy(t *testing.T) {
	existing := map[string]any{"pool": "mac-mini"}
	_ = policyWithDefaultTools(existing, toolset.Default())
	if _, leaked := existing["default_tools"]; leaked {
		t.Fatal("policyWithDefaultTools wrote into the caller's map")
	}
}

// TestTheGrantAcceptsAnAbsentPolicy — a freshly provisioned project has no config_policy at all, and
// a nil map is where a merge written for the happy path panics.
func TestTheGrantAcceptsAnAbsentPolicy(t *testing.T) {
	merged := policyWithDefaultTools(nil, toolset.Default())
	if !reflect.DeepEqual(merged["default_tools"], toolset.Default()) {
		t.Fatalf("default_tools = %v on a nil policy, want the canonical set", merged["default_tools"])
	}
}

// TestTheGrantSkipsAPolicyThatAlreadyGrantsTools drives the predicate over THE SHAPES A JSON DECODE
// ACTUALLY PRODUCES, which is the whole reason it is worth a test.
//
// The skip exists so a re-run cannot overwrite a baseline an operator chose. What makes it fragile is
// that the value is read out of a `map[string]any` decoded from the wire, so a list of names arrives as
// `[]any` of `string` and NEVER as `[]string` — a predicate written against the Go type would assert
// false on every populated policy, skip nothing, and overwrite the operator's list on every bring-up.
// The failure is invisible in the direction that matters: the tool set stays plausible, it is simply
// no longer the one that was set.
//
// The null case is the second shape: `configPolicyInput` marshals its slices WITHOUT omitempty
// (apps/control-plane/internal/identity/store.go:559-565), so a project whose policy was written for
// `pool` alone stores `"default_tools":null` — the key is PRESENT and grants nothing, so a presence
// check would skip the write on exactly the project that needs it.
func TestTheGrantSkipsAPolicyThatAlreadyGrantsTools(t *testing.T) {
	for _, tc := range []struct {
		name   string
		policy map[string]any
		want   bool
	}{
		{"a decoded list of names", map[string]any{"default_tools": []any{"palai.workspace.file"}}, true},
		{"an empty decoded list", map[string]any{"default_tools": []any{}}, false},
		{"the key present and null", map[string]any{"default_tools": nil, "pool": "mac-mini"}, false},
		{"a policy that names other keys", map[string]any{"pool": "mac-mini"}, false},
		{"no policy at all", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := policyAlreadyGrantsTools(tc.policy); got != tc.want {
				t.Fatalf("policyAlreadyGrantsTools(%v) = %v, want %v", tc.policy, got, tc.want)
			}
		})
	}
}

// newProjectStub serves one GET /v1/projects/prj_local carrying `policy` as the config_policy, and
// returns a client pointed at it. `policy` of nil serves a 500 instead, for the unreadable case.
func newProjectStub(t *testing.T, policy map[string]any) *apiClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if policy == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "prj_local", "config_policy": policy})
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "apik_stub", http: srv.Client()}
}

// TestAStackThatAlreadyGrantsToolsIsToldWhatItIsMissing reproduces the LIVE STACK measured on
// 2026-08-04, which is the whole reason this notice exists: prj_local granted seven tools, the build
// shipped nine canonical ones, and five of them — including every tool from that week's search and
// editing work — were absent. `palai up` said NOTHING, because grantDefaultToolBaseline stands down
// the moment a project grants anything at all. On every stack that has ever been brought up, that is
// always. A tool added to the canonical set therefore reached an existing deployment NEVER.
func TestAStackThatAlreadyGrantsToolsIsToldWhatItIsMissing(t *testing.T) {
	live := []string{ // the seven the live stack actually carried
		"palai.workspace.file", "palai.workspace.shell", "palai.workspace.show_media",
		"palai.workspace.commit", "palai.workspace.background_kill",
		"palai.publish.push", "palai.publish.pull_request",
	}
	notice := newProjectStub(t, map[string]any{"default_tools": live}).missingCanonicalToolsNotice()
	if notice == "" {
		t.Fatal("a stack missing five canonical tools was told nothing")
	}
	for _, want := range []string{
		"str_replace_based_edit_tool", "palai.workspace.glob", "palai.workspace.grep",
		"palai.research.fetch", "palai.knowledge.retrieve",
	} {
		if !strings.Contains(notice, want) {
			t.Errorf("the notice does not name the missing tool %q", want)
		}
	}
}

// TestTheSuggestedCommandKeepsWhatTheStackAlreadyHas is the destructive half, and it is the one worth
// having: the canonical set carries NO publish tool, so a notice that told the operator to write the
// canonical list would talk them into deleting their stack's ability to push and open pull requests.
// The command must ADD.
func TestTheSuggestedCommandKeepsWhatTheStackAlreadyHas(t *testing.T) {
	live := []string{"palai.workspace.file", "palai.publish.push", "palai.publish.pull_request"}
	notice := newProjectStub(t, map[string]any{"default_tools": live}).missingCanonicalToolsNotice()
	command := notice[strings.Index(notice, "--default-tools="):]
	for _, keep := range live {
		if !strings.Contains(command, keep) {
			t.Errorf("the suggested command drops %q, which the stack already grants", keep)
		}
	}
}

// TestAFullyGrantedStackIsNotNagged — a notice that fires on a correct stack is one an operator
// learns to ignore, and then the real one is invisible too.
func TestAFullyGrantedStackIsNotNagged(t *testing.T) {
	if n := newProjectStub(t, map[string]any{"default_tools": toolset.Default()}).missingCanonicalToolsNotice(); n != "" {
		t.Errorf("a stack granting the whole canonical set was nagged anyway: %s", n)
	}
}

// TestAnEmptyOrUnreadableBaselineSaysNothingHere — the empty case belongs to emptyToolBaselineWarning,
// which says considerably more; two notices about one condition is how an operator learns to skim. An
// unreadable project is silent for the reason recorded on that function: a notice that cannot tell
// "absent" from "unreadable" is worse than none.
func TestAnEmptyOrUnreadableBaselineSaysNothingHere(t *testing.T) {
	if n := newProjectStub(t, map[string]any{"default_tools": []string{}}).missingCanonicalToolsNotice(); n != "" {
		t.Errorf("the empty baseline was reported twice: %s", n)
	}
	if n := newProjectStub(t, nil).missingCanonicalToolsNotice(); n != "" {
		t.Errorf("an unreadable project produced a notice: %s", n)
	}
}

// recordingStub serves GET /v1/model-connections with `existing` rows and records every POST it
// receives, so a test can assert BOTH calls of the seal-then-name flow actually happened.
func recordingStub(t *testing.T, existing int) (*apiClient, *[]string) {
	t.Helper()
	var posts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			body, _ := io.ReadAll(r.Body)
			posts = append(posts, r.URL.Path+" "+string(body))
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"x"}`))
			return
		}
		rows := make([]map[string]any, existing)
		for i := range rows {
			rows[i] = map[string]any{"id": "mconn_existing"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "apik_stub", http: srv.Client()}, &posts
}

// TestABringUpCredentialIsSealedIntoTheStoreNotTheEnvironment is the other half of removing the
// environment fallback, and without it that removal is a regression: `palai up` still reads
// OPENAI_API_KEY and still writes the 0600 file secret, but nothing reads that file any more. An
// operator would set the variable, watch the bring-up accept it, and get a run that reaches no
// provider — a credential path that is accepted and then ignored is worse than one that is refused.
func TestABringUpCredentialIsSealedIntoTheStoreNotTheEnvironment(t *testing.T) {
	client, posts := recordingStub(t, 0)
	if line := client.seedModelConnection("sk-bring-up-seed-value"); line == "" {
		t.Fatal("a bring-up with a credential and no connection seeded nothing")
	}
	// ‼️ THE COUNT WAS A PROXY AND IT BROKE ON A CORRECT CHANGE. This asserted "exactly 2 POSTs" as a
	// stand-in for "the value goes to the seal and nowhere else" — so publishing the model route, which
	// the sealed credential is USELESS without, failed a test about credential handling. The property is
	// asserted directly now: the value appears in ONE body, and it is the seal's.
	sealed := 0
	for _, call := range *posts {
		if !strings.Contains(call, "sk-bring-up-seed-value") {
			continue
		}
		sealed++
		if !strings.HasPrefix(call, "/v1/secret-refs ") {
			t.Errorf("the credential VALUE appears in %q — every other call must name the ref instead", call)
		}
	}
	if sealed != 1 {
		t.Fatalf("the credential value appears in %d bodies, want exactly 1 (the seal): %v", sealed, *posts)
	}
	// The ORDER still matters: a connection naming a ref that has not been sealed yet is a connection
	// pointing at nothing.
	if !strings.HasPrefix((*posts)[0], "/v1/secret-refs ") {
		t.Errorf("the first call is %q, want the secret to be SEALED first", (*posts)[0])
	}
	if len(*posts) < 2 || !strings.HasPrefix((*posts)[1], "/v1/model-connections ") {
		t.Errorf("the second call is %v, want the connection to NAME the ref", *posts)
	}
	// AND THE ROUTE IS PUBLISHED, because a connection nothing routes to is a credential no run can
	// reach: a project's effective route is its published one, and the deployment default names an
	// unqualified ref that RouteSecretResolver can no longer redeem (the env bridge is gone).
	var published bool
	for _, call := range *posts {
		if strings.Contains(call, "/publish") {
			published = true
		}
	}
	if !published {
		t.Fatalf("the bring-up sealed a credential and bound a connection but published NO route: every "+
			"run on this stack falls to the deployment default, which redeems nothing (%v)", *posts)
	}
}

// TestAnExistingConnectionIsNeverOverwritten — a deployment that already has one was configured by a
// human on the console. A bring-up that re-seeded it would replace a considered choice with whatever
// sits in a dotenv file, silently, on every run of the command.
func TestAnExistingConnectionIsNeverOverwritten(t *testing.T) {
	client, posts := recordingStub(t, 1)
	if line := client.seedModelConnection("sk-would-clobber"); line != "" {
		t.Errorf("a stack with a connection was re-seeded: %s", line)
	}
	if len(*posts) != 0 {
		t.Errorf("%d write(s) went out against an already-configured deployment: %v", len(*posts), *posts)
	}
}

// TestAnUnreadableConnectionListDoesNotSeed — "cannot tell" is not "has none". If the read fails, the
// safe move is to do nothing: seeding over an existing connection is the one outcome this must never
// produce, and a failed GET cannot rule it out.
func TestAnUnreadableConnectionListDoesNotSeed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	client := &apiClient{baseURL: srv.URL, key: "apik_stub", http: srv.Client()}
	if line := client.seedModelConnection("sk-value"); line != "" {
		t.Errorf("seeded against a deployment whose state could not be read: %s", line)
	}
}

// TestTheBringUpFallbackModelMatchesTheControlPlanes binds two strings that live in binaries sharing no
// package: the model a published bring-up route names, and the one modelBrokerFromEnv falls back to when
// PALAI_MODEL is unset.
//
// If they drift, a deployment behaves differently depending on whether a route happens to exist — the
// same stack, the same env, two models — and nothing anywhere would say so.
func TestTheBringUpFallbackModelMatchesTheControlPlanes(t *testing.T) {
	body, err := os.ReadFile("../../../../apps/control-plane/cmd/palai-control-plane/main.go")
	if err != nil {
		t.Fatal(err)
	}
	// modelBrokerFromEnv's own fallback, read out of the source rather than restated here — restating it
	// would make this test agree with itself.
	const fallback = `		model := os.Getenv("PALAI_MODEL")
		if model == "" {
			model = "` + bringUpFallbackModel + `"
		}`
	if !strings.Contains(string(body), fallback) {
		t.Fatalf("the control plane's unset-PALAI_MODEL fallback is not %q — a bring-up route naming that "+
			"model would send runs somewhere the deployment default does not", bringUpFallbackModel)
	}
}
