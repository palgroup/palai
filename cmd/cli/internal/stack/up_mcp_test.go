package stack

import (
	"strings"
	"testing"
)

// E22 T6: `palai up` learns SLACK_AGENT_MCP, the rider that decides which MCP connections a Slack run may
// reach. Until now the rider was NOT SET AT ALL and that was deliberate — a Slack run reached no MCP server
// unless somebody wrote the revision by hand, which is the fail-closed default. This variable keeps the
// default and gives the operator one legible way to change it.
//
// SLACK_AGENT_TOOLS solved this exact problem already, so the shape is copied rather than reinvented: a
// comma-separated list of NAMES, `none` as the explicit disarm, and blank treated as unset.

// FAIL-CLOSED IS THE DEFAULT, and it stays the default. A bring-up that named a connection on its own would
// hand every workspace member a path out to a third-party server nobody chose.
func TestABringUpNamesNoMCPConnectionByDefault(t *testing.T) {
	if got := slackAgentMCP(envGetter(nil)); len(got) != 0 {
		t.Fatalf("a default bring-up named %v — the mcp_connections rider is the capability ceiling and its "+
			"absence is the fail-closed default (extensions/lookup.go)", got)
	}
}

func TestAnOperatorNamesMCPConnectionsByName(t *testing.T) {
	got := slackAgentMCP(envGetter(map[string]string{"SLACK_AGENT_MCP": " jira , confluence "}))
	if len(got) != 2 || got[0] != "jira" || got[1] != "confluence" {
		t.Fatalf("SLACK_AGENT_MCP = %v, want exactly the two named connections with whitespace trimmed", got)
	}
}

// `none` and blank both land on the empty rider — but they are not the same statement, and the word has to
// keep working for the same reason it does on SLACK_AGENT_TOOLS: an operator who wants to say "deliberately
// nothing" should not have to say it by deleting a line.
func TestNoneDisarmsTheMCPRiderAndBlankIsUnset(t *testing.T) {
	for _, raw := range []string{"none", "NONE", "  "} {
		if got := slackAgentMCP(envGetter(map[string]string{"SLACK_AGENT_MCP": raw})); len(got) != 0 {
			t.Fatalf("SLACK_AGENT_MCP=%q named %v, want nothing", raw, got)
		}
	}
}

// THE SILENT-INERT GUARD, and this is the whole reason the rider is not just passed through on create.
// `palai up` REUSES a published revision, so an operator who sets SLACK_AGENT_MCP on an already-provisioned
// stack would get a green bring-up and the OLD, rider-less revision — a Jira connection that resolves to
// nothing, forever, with nothing anywhere saying why.
//
// That defect has shipped twice in this file's history: E21 T4's tool list and E21 T2's registration skip.
// publishedAgentRevision already compares the tool list for exactly this reason; the rider gets the same
// discipline.
func TestChangingTheMCPRiderMintsANewRevisionRatherThanSilentlyReusingTheOld(t *testing.T) {
	api, _ := fakeProvisioningAPI(t)
	first, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveRunTarget(api, envGetter(map[string]string{"SLACK_AGENT_MCP": "jira"}), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision == second.revision {
		t.Fatal("the bring-up reused the rider-less revision after SLACK_AGENT_MCP was set: the setting would " +
			"be silently inert on every stack that has already been provisioned once, and the Jira tool would " +
			"resolve to ErrUnknownTool forever")
	}
	// AND IT SAYS WHAT IT GRANTED. Reaching a third-party server is the kind of grant an operator must be
	// able to read off the bring-up rather than reconstruct from a database.
	if !strings.Contains(second.resolved, "jira") {
		t.Fatalf("the bring-up does not name the connections it granted: %q", second.resolved)
	}
}

// The other direction, which the guard above cannot show on its own: an UNCHANGED rider must still reuse.
// A comparison that never matches would mint a fresh published revision on every single bring-up — the
// orphan-pile outcome ensureSlackAgentRevision's own comment exists to prevent, and the exact shape the
// missing `tools` field produced on the running stack in E21.
func TestAnUnchangedMCPRiderReusesTheRevision(t *testing.T) {
	api, calls := fakeProvisioningAPI(t)
	env := map[string]string{"SLACK_AGENT_MCP": "jira"}
	first, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	before := len(*calls)
	second, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision != second.revision {
		t.Fatal("an unchanged SLACK_AGENT_MCP minted a second revision — every bring-up would leave one behind")
	}
	for _, c := range (*calls)[before:] {
		if strings.HasPrefix(c, "POST ") {
			t.Fatalf("an unchanged rider wrote %q", c)
		}
	}
}

// Reordering is not a change of posture, for the same reason it is not one for the tool list.
func TestReorderingTheMCPRiderIsNotAChange(t *testing.T) {
	api, _ := fakeProvisioningAPI(t)
	env := map[string]string{"SLACK_AGENT_MCP": "jira,confluence"}
	first, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	env["SLACK_AGENT_MCP"] = "confluence,jira"
	second, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision != second.revision {
		t.Fatal("reordering SLACK_AGENT_MCP minted a new revision — the rider is a SET, not a sequence")
	}
}

// The CLI's dependency on the SERVER serving the rider back, proven the way E21 proved it for `tools`:
// drive the fixture to a server that omits the field and show the reuse check stops working. Without this
// the three tests above would pass against a fixture that is more generous than the API.
func TestReuseNeedsTheListToCarryTheMCPRider(t *testing.T) {
	api, _, listCarriesConfig := fakeProvisioningAPIWithTools(t)
	*listCarriesConfig = false

	env := map[string]string{"SLACK_AGENT_MCP": "jira"}
	first, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	second, err := resolveRunTarget(api, envGetter(env), false)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if first.revision == second.revision {
		t.Fatal("this test is vacuous: with no mcp_connections in the list the bring-up should NOT have been " +
			"able to recognise its own revision, yet it reused one")
	}
}

// A bring-up that names NO connection still says so, because "the agent cannot reach Jira" is exactly the
// state an operator otherwise discovers as an agent that keeps answering as if Jira does not exist.
func TestTheBringUpSaysWhenNoMCPConnectionIsNamed(t *testing.T) {
	api, _ := fakeProvisioningAPI(t)
	target, err := resolveRunTarget(api, envGetter(nil), false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(target.resolved, "SLACK_AGENT_MCP") {
		t.Fatalf("a rider-less bring-up says %q — it must name the variable that changes it, or reaching Jira "+
			"is a state an operator can only discover by reading the source", target.resolved)
	}
}
