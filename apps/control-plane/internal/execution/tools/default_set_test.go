package tools

import (
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/packages/toolset"
)

// THE DEFECT THIS FILE EXISTS FOR, shipped and caught the same day (E21 T5): the workspace search tool was
// written, wired, tested and mounted — and could never be advertised, because `palai up`'s default tool list
// did not name it. advertisedTools iterates the run's EFFECTIVE SET and only asks the broker about names it
// already holds, so a tool absent from that list is never resolved however completely it is built. The whole
// feature was dead and every test around it was green.
//
// The canonical lists are read from packages/toolset, NOT parsed out of the CLI's source. The regex this
// replaces asserted the shape of a Go declaration in a file three directories away: it went RED the day
// commit 1e5fc63e deleted that declaration, and it would have gone silently WRONG the day somebody
// reformatted it. An import cannot drift from what the CLI actually grants, because it is what the CLI
// actually grants.
//
// THREE LISTS, and the guard covers ALL of them: toolset.Default() is what every bring-up binds,
// toolset.Repository() is the coding half a bring-up ADDS when it bound a repository, and toolset.Publish()
// is the publication half added under the same condition. A name that resolves in none of them is a tool the
// model would be offered and could never be given — the exact defect this file was written for, and a
// conditional list is a second place for it to happen.

// TestEveryDefaultToolResolves is the guard proper: every name `palai up` binds must be a tool this
// control plane can actually produce, either from the static broker set or from the Slack-search lookup.
func TestEveryDefaultToolResolves(t *testing.T) {
	authorities := NewSearchAuthorities()
	authorities.Grant("run_guard", "T1", "https://slack.test/api", []byte("t"), "act")
	env := toolbroker.ExecEnv{Scope: toolbroker.TaskScope{RunID: "run_guard"}}

	broker := toolbroker.New(
		ResearchFetchTool(),
		KnowledgeRetrievalTool(nil),
		// The coding three, mounted here because E22 T3's bring-up binds them when it bound a repository.
		// They are in the SAME broker as the read-only three on purpose: production builds one broker, and a
		// guard that resolved each list against its own hand-picked set would prove less than it appears to.
		FileTool(),
		ShellTool(),
		CommitTool(),
		// And the fourth (E26 T2): the shell tool's `background` parameter can start a task, so the tool that
		// stops one is granted under the same condition.
		BackgroundKillTool(),
		// And the fifth: the agent's way to SHOW the human a screenshot or a recording it produced. It is
		// bound under the same condition as the coding tools because it reads from the same workspace — a
		// run with no repository has nothing to screenshot.
		MediaTool(),
		// And the publish two, which E22 T4's bring-up binds under the same condition. They are in the SAME
		// broker for the same reason — main.go builds one (main.go:459-467), and a guard that gave each list
		// its own hand-picked set would prove less than it appears to.
		PushTool(),
		PullRequestTool(),
		MergeTool(),
	)
	broker.SetLookup(SlackSearchLookup(nil, authorities, nil))

	names := toolset.Default()
	names = append(names, toolset.Repository()...)
	names = append(names, toolset.Publish()...)
	for _, name := range names {
		if _, found, err := broker.SchemaResolved(t.Context(), env, name); err != nil || !found {
			t.Fatalf("`palai up` binds %q and this control plane cannot resolve it (found=%v err=%v). "+
				"advertisedTools only asks about names in the effective set, so the model would never be "+
				"offered it — the tool would be mounted and dead", name, found, err)
		}
	}
}

// TestThePublishToolsAreTheirOwnListAndNeitherPublishes is the control-plane side of E22 T4, and it holds
// two things a single merged list would have made unreadable.
//
// FIRST, the lists stay SEPARATE. `palai up` adds both under one condition, but the coding half is what an
// agent does to a workspace nobody else can see and the publish half is what leaves the machine. Keeping the
// second one nameable is what let T3 ship the first without the second at all.
//
// SECOND, and this is the invariant the whole task rests on: NEITHER publish tool acts. Each records a
// pending publication and answers pending_approval, which is why granting them is not granting a push. The
// assertion reads the shipped tools rather than the comment above them — if someone made pushExec push, the
// ReplayClass would still say idempotent and this test is what would notice the description stopped being
// true. The behavioural proof is publish_test.go (the tool returns pending_approval and calls no publisher);
// what is checked here is that the names in the canonical publish list are those tools and nothing else.
func TestThePublishToolsAreTheirOwnListAndNeitherPublishes(t *testing.T) {
	for _, name := range toolset.Repository() {
		if strings.HasPrefix(name, "palai.publish.") {
			t.Fatalf("the CODING tool list grants %q: the publish half has its own list (toolset.Publish) so "+
				"that granting a workspace and granting a publication stay two decisions", name)
		}
	}
	publish := toolset.Publish()
	byName := map[string]toolbroker.Tool{
		PushTool().Name:        PushTool(),
		PullRequestTool().Name: PullRequestTool(),
		MergeTool().Name:       MergeTool(),
	}
	if len(publish) != len(byName) {
		t.Fatalf("toolset.Publish() = %v, want exactly the %d publication tools this control plane mounts", publish, len(byName))
	}
	for _, name := range publish {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("toolset.Publish() names %q, which is not one of this control plane's publication tools — a "+
				"third publish tool is a third approval surface and must be an explicit decision", name)
		}
		if !strings.Contains(tool.Description, "approval") {
			t.Fatalf("%s no longer tells the model its request is gated: %q. The description is what the model "+
				"reads before it decides to call it", name, tool.Description)
		}
	}
}

// TestNoDefaultToolPublishes keeps the half of the old side-effect guard that still stands: nothing a
// bring-up binds — with a repository or without one — may be a tool that moves work OFF the machine.
// Publication is the one capability gated by an explicit human decision (E23 T6), and toolset.Publish() is
// the separate list that carries it.
//
// NARROWED AND RENAMED 2026-08-04, both for the same reason. This test used to forbid `workspace.file` and
// `workspace.shell` in the default set as well, and it was called TestNoDefaultToolHasSideEffects. Commit
// 1e5fc63e deleted `palai up`'s Slack wiring and the agent tool lists with it, and the posture that clause
// defended belonged to that world: the grant of a bot anyone in a workspace can DM, not the baseline of a
// bring-up. Those two names are now in toolset.Default() DELIBERATELY (toolset.go:14-25) — a bring-up that
// cannot read a file or run a command is the defect this guard's own plan exists to fix. The old remedy
// sentence told the reader to make "an explicit SLACK_AGENT_TOOLS decision"; that variable's only occurrence
// in the whole tree was this message string, so it named a mechanism no reader could act on. And the old
// NAME is why the rename is not cosmetic: `workspace.shell` runs arbitrary commands, so a test asserting
// "no default tool has side effects" would now print a PASS for a claim that is false.
//
// What survives is not a leftover. A file write and a command are bounded by the workspace; a push, a pull
// request and a merge are not, and a default that granted one would be a posture change nobody chose.
func TestNoDefaultToolPublishes(t *testing.T) {
	for _, list := range []struct {
		source string
		names  []string
	}{
		{"toolset.Default()", toolset.Default()},
		{"toolset.Repository()", toolset.Repository()},
	} {
		for _, name := range list.names {
			if strings.Contains(name, "publish.") {
				t.Fatalf("%s grants %q — publication leaves the machine, so it stays in toolset.Publish() "+
					"behind the human decision that gates it, and is never bound by a default", list.source, name)
			}
		}
	}
}
