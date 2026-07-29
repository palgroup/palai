package stack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// E22 T3: `palai up` binds the repository a Slack thread codes against, and the tool list it grants follows
// from whether it managed to. Two failure shapes this file exists to refuse, both of which this repository
// has already shipped once:
//
//   - the SILENT SKIP (E21 T2): configuration absent, bring-up green, agent quietly unable to do the thing
//     the operator installed it for.
//   - the ADVERTISED-BUT-DEAD TOOL (E21 T5): a tool in the list whose every call ends in "no workspace bound
//     for this run" — worse than no tool, because the model keeps trying it.

func TestABringUpGrantsWorkspaceToolsOnlyWithARepositoryBinding(t *testing.T) {
	without := slackAgentTools(envGetter(nil), false)
	for _, name := range without {
		for _, forbidden := range []string{"workspace.", "publish."} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("a bring-up that bound NO repository granted %q: every call to it would answer "+
					"'no workspace bound for this run', which is worse than not offering it at all", name)
			}
		}
	}

	with := slackAgentTools(envGetter(nil), true)
	for _, want := range []string{"palai.workspace.file", "palai.workspace.shell", "palai.workspace.commit",
		"palai.publish.push", "palai.publish.pull_request", "palai.publish.merge_pull_request"} {
		if !contains(with, want) {
			t.Fatalf("a bring-up that bound a repository did NOT grant %q, so the agent cannot write code in a "+
				"workspace that exists: %v", want, with)
		}
	}
	// The read-only three survive the widening: a repository does not replace research, knowledge or search.
	for _, want := range slackDefaultTools {
		if !contains(with, want) {
			t.Fatalf("binding a repository DROPPED %q from the list: %v", want, with)
		}
	}
}

// THE APPROVAL BOUNDARY IS STRUCTURAL, NOT A CONFIGURATION ACCIDENT — and E22 T4 is where that sentence
// gets tested from the other side. T3 asserted that binding a repository granted NO publish tool, because
// publishing had no approval path wired from Slack yet. It has one now, so the assertion is INVERTED rather
// than deleted, and what it holds is the part that never moved:
//
//   - a bring-up with a repository grants the two publish tools, so the agent can ASK to push;
//   - a bring-up WITHOUT one grants neither, because a publication with no binding has no destination;
//   - and neither tool is a push. Both record a pending publication and return pending_approval; the push
//     happens in the approval pump, after a human in SLACK_APPROVER_IDS presses Approve.
//
// So the boundary is still a human's button. What changed is that the button is now reachable.
func TestABringUpWithARepositoryGrantsThePublishToolsAndOneWithoutGrantsNone(t *testing.T) {
	var granted []string
	for _, name := range slackAgentTools(envGetter(nil), true) {
		if strings.HasPrefix(name, "palai.publish.") {
			granted = append(granted, name)
		}
	}
	if len(granted) != 3 || !contains(granted, "palai.publish.push") || !contains(granted, "palai.publish.pull_request") ||
		!contains(granted, "palai.publish.merge_pull_request") {
		t.Fatalf("a repository binding granted publish tools %v, want exactly push + pull_request + merge — the "+
			"agent must be able to ASK, and asking is all any of them does", granted)
	}
	for _, name := range slackAgentTools(envGetter(nil), false) {
		if strings.HasPrefix(name, "palai.publish.") {
			t.Fatalf("a bring-up that bound NO repository granted %q: with no binding RunPublicationTarget "+
				"answers 'the run prepared no repository', so the tool could only ever fail", name)
		}
	}
}

// SLACK_AGENT_TOOLS still wins WHOLESALE, repository or not: what an operator granted stays legible in one
// line rather than being a default plus a delta they have to compute.
func TestSlackAgentToolsWinsWholesaleEvenWithARepository(t *testing.T) {
	got := slackAgentTools(envGetter(map[string]string{"SLACK_AGENT_TOOLS": "palai.research.fetch"}), true)
	if len(got) != 1 || got[0] != "palai.research.fetch" {
		t.Fatalf("SLACK_AGENT_TOOLS = %v with a repository bound, want exactly the one named tool — an explicit "+
			"list that silently gained three more would make what an operator granted unreadable", got)
	}
}

// TestABringUpBindsTheRepositoryAndSaysWhatItMade: the binding is created BY this command (§0.2 asks the
// operator for a clone URL and a base branch, not for a Palai id nobody can obtain yet), and the report names
// what it made — including the base branch, because "open the PR against dev" IS that value.
func TestABringUpBindsTheRepositoryAndSaysWhatItMade(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	repo := resolveRepository(api, envGetter(map[string]string{
		"PALAI_GIT_REPO":        "acme/widgets",
		"PALAI_GIT_CLONE_URL":   "https://github.com/acme/widgets.git",
		"PALAI_GIT_BASE_BRANCH": "dev",
	}))
	if repo.warn != "" {
		t.Fatalf("a fully configured repository still warned: %q", repo.warn)
	}
	if repo.id == "" {
		t.Fatal("no repository binding was created, so the Slack connection has nothing to bind and the agent " +
			"cannot clone anything")
	}
	if !strings.Contains(repo.resolved, repo.id) || !strings.Contains(repo.resolved, "acme/widgets") ||
		!strings.Contains(repo.resolved, "dev") {
		t.Fatalf("the bring-up does not say what it made: %q — an operator needs the binding id, the repository "+
			"and the base branch a pull request will target", repo.resolved)
	}
	posted := 0
	for _, c := range *calls {
		if c == "POST /v1/repository-bindings" {
			posted++
		}
	}
	if posted != 1 {
		t.Fatalf("%d POSTs to /v1/repository-bindings, want 1", posted)
	}
}

// A re-run must REUSE. `palai up` is run constantly and a binding is not an idempotent operation: a fresh one
// per bring-up would pile up duplicates and move the connection's repository underneath a live thread.
func TestABringUpReusesTheRepositoryBindingItAlreadyMade(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	env := map[string]string{
		"PALAI_GIT_CLONE_URL":   "https://github.com/acme/widgets.git",
		"PALAI_GIT_BASE_BRANCH": "dev",
	}
	first := resolveRepository(api, envGetter(env))
	if first.id == "" {
		t.Fatalf("first resolve bound nothing: %q", first.warn)
	}
	before := len(*calls)
	second := resolveRepository(api, envGetter(env))
	if second.id != first.id {
		t.Fatalf("a second bring-up bound %s after %s: the repository would move under the operator, and a "+
			"thread mid-conversation would change repository", second.id, first.id)
	}
	for _, c := range (*calls)[before:] {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("an unchanged repository configuration wrote %q", c)
		}
	}
	if !strings.Contains(second.resolved, "reused") {
		t.Fatalf("the second bring-up reports its reused binding as though it made it: %q", second.resolved)
	}

	// Retargeting the base branch is a REAL change and must not be silently inert — the same failure shape
	// publishedAgentRevision refuses for the tool list. A binding still saying `main` after the operator asked
	// for `dev` would open every pull request against the wrong branch.
	env["PALAI_GIT_BASE_BRANCH"] = "main"
	retargeted := resolveRepository(api, envGetter(env))
	if retargeted.id == first.id {
		t.Fatal("changing PALAI_GIT_BASE_BRANCH reused the binding that carries the OLD branch: every pull " +
			"request would target a branch the operator stopped asking for, with a green bring-up saying so")
	}
}

// TestAMissingRepositoryConfigurationWarnsRatherThanSkippingSilently is E21 T2's lesson, and this file has
// now learned it twice. Half-configured is warned about too: one of the two values is the state an operator
// reaches by editing .env.local and stopping halfway.
func TestAMissingRepositoryConfigurationWarnsRatherThanSkippingSilently(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	for _, tc := range []struct {
		name string
		env  map[string]string
	}{
		{"neither", nil},
		{"clone url only", map[string]string{"PALAI_GIT_CLONE_URL": "https://github.com/acme/widgets.git"}},
		{"base branch only", map[string]string{"PALAI_GIT_BASE_BRANCH": "dev"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := resolveRepository(api, envGetter(tc.env))
			if repo.id != "" {
				t.Fatalf("a %s configuration bound %s anyway", tc.name, repo.id)
			}
			if repo.warn == "" {
				t.Fatalf("a %s configuration skipped SILENTLY inside a green bring-up: the operator's only other "+
					"symptom is an agent that keeps answering 'no workspace bound for this run'", tc.name)
			}
			if !strings.Contains(repo.warn, "PALAI_GIT_") {
				t.Fatalf("the warning does not name the variable to set: %q", repo.warn)
			}
		})
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("an unconfigured repository still wrote %q", c)
		}
	}
}

// A control-plane that mounts NO repository-binding surface is a real state — api.NewRouter leaves those
// three routes unmounted when it is wired no BindingRegistrar — and it must be a WARNING, not a swallowed
// error and not a failed bring-up. An absent repository is a legitimate posture; an absent repository the
// operator was never told about is the silent skip again.
func TestARepositorySurfaceTheStackDoesNotMountIsReportedRatherThanSwallowed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"detail":"no such route"}`))
	}))
	t.Cleanup(srv.Close)
	api := &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}

	repo := resolveRepository(api, envGetter(map[string]string{
		"PALAI_GIT_CLONE_URL":   "https://github.com/acme/widgets.git",
		"PALAI_GIT_BASE_BRANCH": "dev",
	}))
	if repo.id != "" {
		t.Fatalf("a stack with no binding surface still reported binding %s", repo.id)
	}
	if repo.warn == "" {
		t.Fatal("a repository the stack could not bind was swallowed: the operator configured one, none exists, " +
			"and nothing said so")
	}
	if !strings.Contains(repo.warn, "repository-bindings") {
		t.Fatalf("the warning does not carry what went wrong: %q", repo.warn)
	}
}

// The identity fallback: an operator who set only PALAI_GIT_CLONE_URL still gets a binding rather than a 400
// on a required field they were never asked for.
func TestTheRepositoryIdentityFallsBackToTheCloneURLPath(t *testing.T) {
	for _, tc := range []struct{ url, want string }{
		{"https://github.com/acme/widgets.git", "acme/widgets"},
		{"https://github.com/acme/widgets", "acme/widgets"},
		{"https://ghe.example.test/team/sub/repo.git", "team/sub/repo"},
	} {
		if got := repositoryIdentityFrom(tc.url); got != tc.want {
			t.Fatalf("repositoryIdentityFrom(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
