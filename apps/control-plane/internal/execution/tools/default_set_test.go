package tools

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// THE DEFECT THIS FILE EXISTS FOR, shipped and caught the same day (E21 T5): the workspace search tool was
// written, wired, tested and mounted — and could never be advertised, because `palai up`'s default tool list
// did not name it. advertisedTools iterates the run's EFFECTIVE SET and only asks the broker about names it
// already holds, so a tool absent from that list is never resolved however completely it is built. The whole
// feature was dead and every test around it was green.
//
// The gap is structural rather than careless: cmd/cli cannot import this package (Go's internal rule), so it
// hardcodes the names, and nothing compared the two sides. This test is that comparison. It lives here
// because this is the only place that can see BOTH the CLI's literal list and the real broker.

// readCLIToolList extracts the names of one `var <name> = []string{...}` from cmd/cli's source. Reading
// source is the price of the internal boundary; the alternative is a shared package that exists only to be
// imported by a test.
//
// TWO LISTS SINCE E22 T3, and the guard covers BOTH: slackDefaultTools is what every bring-up binds, and
// slackRepositoryTools is what a bring-up ADDS when it bound a repository. A name that resolves in neither
// is a tool the model would be offered and could never be given — the exact defect this file was written for,
// and a conditional list is a second place for it to happen.
func readCLIToolList(t *testing.T, name string) []string {
	t.Helper()
	// tools -> execution -> internal -> control-plane -> apps -> repo root
	path := filepath.Join("..", "..", "..", "..", "..", "cmd", "cli", "internal", "stack", "up.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — if this file moved, this guard must follow it rather than be deleted", path, err)
	}
	m := regexp.MustCompile(`var ` + name + ` = \[\]string\{([^}]*)\}`).FindSubmatch(src)
	if m == nil {
		t.Fatalf("%s was not found in cmd/cli/internal/stack/up.go: the guard cannot be allowed to pass by "+
			"failing to find what it checks", name)
	}
	var names []string
	for _, raw := range strings.Split(string(m[1]), ",") {
		if name := strings.Trim(strings.TrimSpace(raw), `"`); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatalf("parsed an EMPTY %s — a guard that finds nothing proves nothing", name)
	}
	return names
}

// readCLISlackDefaultTools is the unconditional list: what a bring-up binds with no repository.
func readCLISlackDefaultTools(t *testing.T) []string {
	t.Helper()
	return readCLIToolList(t, "slackDefaultTools")
}

// TestEverySlackDefaultToolResolves is the guard proper: every name `palai up` binds must be a tool this
// control plane can actually produce, either from the static broker set or from the Slack-search lookup.
func TestEverySlackDefaultToolResolves(t *testing.T) {
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
	)
	broker.SetLookup(SlackSearchLookup(nil, authorities, nil))

	names := readCLISlackDefaultTools(t)
	names = append(names, readCLIToolList(t, "slackRepositoryTools")...)
	for _, name := range names {
		if _, found, err := broker.SchemaResolved(t.Context(), env, name); err != nil || !found {
			t.Fatalf("`palai up` binds %q and this control plane cannot resolve it (found=%v err=%v). "+
				"advertisedTools only asks about names in the effective set, so the model would never be "+
				"offered it — the tool would be mounted and dead", name, found, err)
		}
	}
}

// TestTheRepositoryToolListStopsShortOfPublishing is the control-plane side of E22 T3's deliberate gap: a
// bring-up that bound a repository grants the tools that WRITE code and none that PUBLISH it. T4 opens the
// publish half, and it opens it behind a human's Approve button — so a publish tool appearing in this list
// would move that boundary by accident, which is exactly how a boundary stops being one.
func TestTheRepositoryToolListStopsShortOfPublishing(t *testing.T) {
	for _, name := range readCLIToolList(t, "slackRepositoryTools") {
		if strings.HasPrefix(name, "palai.publish.") {
			t.Fatalf("the repository tool list grants %q: binding a repository must not, on its own, grant the "+
				"ability to push or open a pull request", name)
		}
	}
}

// TestTheSearchToolIsInTheDefaultSet is the specific half, kept separate from the general guard because it
// states the intent rather than the invariant: the search tool must SHIP ON, and its two real gates are
// elsewhere (the workspace granting search:read.public, and the run carrying an action_token). Leaving it
// out of the list would disable the feature in a way that looks like configuration rather than a bug.
func TestTheSearchToolIsInTheDefaultSet(t *testing.T) {
	for _, name := range readCLISlackDefaultTools(t) {
		if name == slackSearchToolName {
			return
		}
	}
	t.Fatalf("%s is not in `palai up`'s default tool list, so no Slack run can ever be offered it. The gates "+
		"that decide whether a search may actually happen are the SCOPE and the action_token, not this list",
		slackSearchToolName)
}

// A default that grants a side-effecting tool would be a posture change nobody chose — anyone in the
// workspace can DM this bot. The CLI has its own version of this test; this one holds the line from the side
// that knows what each tool DOES.
func TestNoDefaultSlackToolHasSideEffects(t *testing.T) {
	for _, name := range readCLISlackDefaultTools(t) {
		for _, forbidden := range []string{"workspace.shell", "workspace.file", "workspace.commit", "publish."} {
			if strings.Contains(name, forbidden) {
				t.Fatalf("the DEFAULT list grants %q — a tool that writes, runs commands or publishes must be "+
					"an explicit SLACK_AGENT_TOOLS decision", name)
			}
		}
	}
}
