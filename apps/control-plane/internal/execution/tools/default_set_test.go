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

var slackDefaultToolsPattern = regexp.MustCompile(`var slackDefaultTools = \[\]string\{([^}]*)\}`)

// readCLISlackDefaultTools extracts the names from cmd/cli's source. Reading source is the price of the
// internal boundary; the alternative is a shared package that exists only to be imported by a test.
func readCLISlackDefaultTools(t *testing.T) []string {
	t.Helper()
	// tools -> execution -> internal -> control-plane -> apps -> repo root
	path := filepath.Join("..", "..", "..", "..", "..", "cmd", "cli", "internal", "stack", "up.go")
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v — if this file moved, this guard must follow it rather than be deleted", path, err)
	}
	m := slackDefaultToolsPattern.FindSubmatch(src)
	if m == nil {
		t.Fatal("slackDefaultTools was not found in cmd/cli/internal/stack/up.go: the guard cannot be allowed " +
			"to pass by failing to find what it checks")
	}
	var names []string
	for _, raw := range strings.Split(string(m[1]), ",") {
		if name := strings.Trim(strings.TrimSpace(raw), `"`); name != "" {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		t.Fatal("parsed an EMPTY default tool list — a guard that finds nothing proves nothing")
	}
	return names
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
	)
	broker.SetLookup(SlackSearchLookup(nil, authorities, nil))

	for _, name := range readCLISlackDefaultTools(t) {
		if _, found, err := broker.SchemaResolved(t.Context(), env, name); err != nil || !found {
			t.Fatalf("`palai up` binds %q and this control plane cannot resolve it (found=%v err=%v). "+
				"advertisedTools only asks about names in the effective set, so the model would never be "+
				"offered it — the tool would be mounted and dead", name, found, err)
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
