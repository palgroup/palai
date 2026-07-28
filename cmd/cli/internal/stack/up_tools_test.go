package stack

import (
	"os"
	"strings"
	"testing"
)

// E21 T4: the revision `palai up` creates now carries a tool list, because a revision with an EMPTY
// effective tool set is single-step — it can answer and it can do nothing else. These tests pin WHICH tools,
// and the pinning matters more than the count: the default is what every workspace member can reach through
// a DM, and the widening is a posture change the operator makes on purpose.

func TestABringUpBindsOnlyReadOnlyToolsByDefault(t *testing.T) {
	got := slackAgentTools(envGetter(nil), false)
	if len(got) == 0 {
		t.Fatal("a default bring-up bound NO tools — the Slack agent would be single-step out of the box")
	}
	// The tools that write, run commands, or publish must NOT be reachable from a DM by default. Anyone in
	// the workspace can message this bot; a default shell is a standing capability nobody opted into.
	for _, name := range got {
		for _, forbidden := range []string{"shell", "commit", "push", "pull_request", "file"} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("the DEFAULT tool list grants %q — a side-effecting tool must be an explicit "+
					"operator decision (SLACK_AGENT_TOOLS), not a default every workspace member inherits", name)
			}
		}
	}
}

func TestAnOperatorWidensTheToolListByName(t *testing.T) {
	got := slackAgentTools(envGetter(map[string]string{
		"SLACK_AGENT_TOOLS": " palai.workspace.shell , palai.workspace.file ",
	}), false)
	if len(got) != 2 || got[0] != "palai.workspace.shell" || got[1] != "palai.workspace.file" {
		t.Fatalf("SLACK_AGENT_TOOLS = %v, want exactly the two named tools with whitespace trimmed", got)
	}
}

// The narrowest posture has to stay reachable, and it needs a spelling that a stray blank line cannot
// produce by accident.
func TestNoneGrantsNothingAndBlankFallsBackToTheDefaults(t *testing.T) {
	if got := slackAgentTools(envGetter(map[string]string{"SLACK_AGENT_TOOLS": "none"}), true); len(got) != 0 {
		t.Fatalf("SLACK_AGENT_TOOLS=none granted %v, want nothing", got)
	}
	if got := slackAgentTools(envGetter(map[string]string{"SLACK_AGENT_TOOLS": "  "}), false); len(got) == 0 {
		t.Fatal("a blank SLACK_AGENT_TOOLS disarmed the agent — blank is unset, and disarming needs the word none")
	}
}

// THE SILENT-INERT GUARD. This is the failure this change could most easily have introduced: `palai up`
// reuses a published revision, so an operator who sets SLACK_AGENT_TOOLS on an already-provisioned stack
// would get a green bring-up and the OLD revision, with their setting doing nothing. Same shape as the
// silent registration SKIP E21 T2 removed from this file.
func TestChangingTheToolListMintsANewRevisionRatherThanSilentlyReusingTheOld(t *testing.T) {
	api, _ := fakeProvisioningAPI(t)
	first, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveRunTarget(api, envGetter(map[string]string{"SLACK_AGENT_TOOLS": "palai.workspace.shell"}), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision == second.revision {
		t.Fatal("the bring-up reused the old revision after the tool list changed: SLACK_AGENT_TOOLS would be " +
			"silently inert on every stack that has already been provisioned once")
	}
	if !strings.Contains(second.resolved, "palai.workspace.shell") {
		t.Fatalf("the bring-up does not say what it granted: %q — an operator needs the tool NAMES, since "+
			"\"1 tool\" does not distinguish a web fetch from a shell", second.resolved)
	}
}

// Reordering is not a change of posture, and treating it as one would mint a revision on every bring-up
// whose .env.local happens to list the same tools differently.
func TestReorderingTheToolListIsNotAChange(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	env := map[string]string{"SLACK_AGENT_TOOLS": "palai.research.fetch,palai.knowledge.retrieve"}
	first, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := len(*calls)
	env["SLACK_AGENT_TOOLS"] = "palai.knowledge.retrieve,palai.research.fetch"
	second, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision != second.revision {
		t.Fatal("reordering SLACK_AGENT_TOOLS minted a new revision — the list is a SET, not a sequence")
	}
	for _, c := range (*calls)[before:] {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("a reorder wrote %q", c)
		}
	}
}

// THE BUG THIS FIXTURE ONCE HID, found on a running stack rather than in a test: the revisions LIST did not
// serialise `tools`, so the reuse check above compared its wanted list against nil, never matched, and
// `palai up` minted a fresh published revision on EVERY bring-up. Two bring-ups left revision 1 and
// revision 2 with identical tool lists — exactly the "binding moves under the operator, orphan revisions
// pile up" outcome ensureSlackAgentRevision's own comment was written to prevent.
//
// The fake was the reason it was invisible: it returned tools in the list when the server did not. This
// test drives the fixture back to the server's OLD shape, so the reuse check's dependency on that field is
// a proven requirement rather than an assumption.
func TestReuseNeedsTheListToCarryTools(t *testing.T) {
	api, calls, listCarriesTools := fakeProvisioningAPIWithTools(t)
	*listCarriesTools = false // the server as it behaved before this was fixed

	first, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := len(*calls)
	second, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision == second.revision {
		t.Fatal("this test is vacuous: with no tools in the list the bring-up should NOT have been able to " +
			"recognise its own revision, yet it reused one")
	}
	minted := false
	for _, c := range (*calls)[before:] {
		if strings.HasPrefix(c, "POST ") && strings.HasSuffix(c, "/revisions") {
			minted = true
		}
	}
	if !minted {
		t.Fatal("expected the second bring-up to mint a revision when it cannot read the first one's tools")
	}
}

// CONCURRENCY IS TWO NUMBERS AND A STACK IS AS PARALLEL AS THE SMALLER ONE. PALAI_DISPATCH_WORKERS is how
// many runs the control plane dispatches at once; PALAI_RUNNER_CONCURRENCY is how many engine leases one
// runner parks at once, and at the default of 1 a second concurrent dial BLOCKS. `palai up` exported the
// first and not the second, so a .env.local asking for four concurrent runs got four dispatch workers
// queueing behind one engine — measured on the live stack 2026-07-28, dispatch=4 and runner=1.
func TestBothConcurrencyKnobsReachTheStack(t *testing.T) {
	for _, tc := range []struct{ name, dispatch, runner, wantRunner string }{
		{"explicit runner value wins", "4", "2", "2"},
		{"unset runner follows the dispatch count", "4", "", "4"},
		{"a lone runner value is honoured", "", "3", "3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PALAI_DISPATCH_WORKERS", "")
			t.Setenv("PALAI_RUNNER_CONCURRENCY", "")
			env := map[string]string{}
			if tc.dispatch != "" {
				env["PALAI_DISPATCH_WORKERS"] = tc.dispatch
			}
			if tc.runner != "" {
				env["PALAI_RUNNER_CONCURRENCY"] = tc.runner
			}
			if err := applyConcurrencyEnv(envGetter(env)); err != nil {
				t.Fatalf("applyConcurrencyEnv: %v", err)
			}
			if got := os.Getenv("PALAI_RUNNER_CONCURRENCY"); got != tc.wantRunner {
				t.Fatalf("PALAI_RUNNER_CONCURRENCY = %q, want %q — compose reads this from the environment, so "+
					"an unexported value leaves the runner at 1 and the stack serial", got, tc.wantRunner)
			}
		})
	}
}
