package stack

import (
	"strings"
	"testing"
)

// E21 T4: the revision `palai up` creates now carries a tool list, because a revision with an EMPTY
// effective tool set is single-step — it can answer and it can do nothing else. These tests pin WHICH tools,
// and the pinning matters more than the count: the default is what every workspace member can reach through
// a DM, and the widening is a posture change the operator makes on purpose.

func TestABringUpBindsOnlyReadOnlyToolsByDefault(t *testing.T) {
	got := slackAgentTools(envGetter(nil))
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
	}))
	if len(got) != 2 || got[0] != "palai.workspace.shell" || got[1] != "palai.workspace.file" {
		t.Fatalf("SLACK_AGENT_TOOLS = %v, want exactly the two named tools with whitespace trimmed", got)
	}
}

// The narrowest posture has to stay reachable, and it needs a spelling that a stray blank line cannot
// produce by accident.
func TestNoneGrantsNothingAndBlankFallsBackToTheDefaults(t *testing.T) {
	if got := slackAgentTools(envGetter(map[string]string{"SLACK_AGENT_TOOLS": "none"})); len(got) != 0 {
		t.Fatalf("SLACK_AGENT_TOOLS=none granted %v, want nothing", got)
	}
	if got := slackAgentTools(envGetter(map[string]string{"SLACK_AGENT_TOOLS": "  "})); len(got) == 0 {
		t.Fatal("a blank SLACK_AGENT_TOOLS disarmed the agent — blank is unset, and disarming needs the word none")
	}
}

// THE SILENT-INERT GUARD. This is the failure this change could most easily have introduced: `palai up`
// reuses a published revision, so an operator who sets SLACK_AGENT_TOOLS on an already-provisioned stack
// would get a green bring-up and the OLD revision, with their setting doing nothing. Same shape as the
// silent registration SKIP E21 T2 removed from this file.
func TestChangingTheToolListMintsANewRevisionRatherThanSilentlyReusingTheOld(t *testing.T) {
	api, _ := fakeProvisioningAPI(t)
	first, err := resolveRunTarget(api, envGetter(nil))
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveRunTarget(api, envGetter(map[string]string{"SLACK_AGENT_TOOLS": "palai.workspace.shell"}))
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
	first, err := resolveRunTarget(api, envGetter(env))
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := len(*calls)
	env["SLACK_AGENT_TOOLS"] = "palai.knowledge.retrieve,palai.research.fetch"
	second, err := resolveRunTarget(api, envGetter(env))
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
