package tools

import (
	"testing"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestEveryToolIsClassifiedForParallelSafety pins the classification by NAMING each tool, because the
// derivation people reach for first is wrong. ReplayClass describes what happens on REPLAY, not
// whether two calls may overlap, and the two disagree in both directions here:
//
//   - `palai.workspace.show_media` is ClassPure and still WRITES an artifact.
//   - `palai.workspace.file` and the text editor are ClassReversible — reversible via snapshot — and
//     must never run beside another write to the same tree.
//
// So the safe set is the tools that read and write nothing at all.
func TestEveryToolIsClassifiedForParallelSafety(t *testing.T) {
	want := map[string]bool{
		// Read-only: no workspace mutation, no external object, nothing another call could observe.
		"palai.workspace.glob":      true,
		"palai.workspace.grep":      true,
		"palai.knowledge.retrieve":  true,
		// Multiplexed read AND write behind one parameter, so the TOOL cannot be safe even though its
		// read path would be. This is a concrete cost of the `op`/`command` shape: splitting reads onto
		// their own tool is what would let them overlap.
		"palai.workspace.file":        false,
		"str_replace_based_edit_tool": false,
		// Writes something, somewhere.
		"palai.workspace.show_media":       false, // writes an artifact
		"palai.workspace.commit":           false,
		"palai.publish.push":               false,
		"palai.publish.pull_request":       false,
		"palai.publish.merge_pull_request": false,
		// Side effects by definition.
		"palai.workspace.shell":           false,
		"palai.workspace.background_kill": false,
		// Fetches over the network. Left serial deliberately rather than by oversight: it is read-only
		// at this end, but concurrent fetches are the shape that trips a remote rate limit, and nothing
		// here measures that yet. Revisit with a measurement, not a guess.
		"palai.research.fetch": false,
	}

	for _, tool := range []toolbroker.Tool{
		FileTool(), TextEditorTool(), GlobTool(), GrepTool(), ShellTool(),
		BackgroundKillTool(), MediaTool(), CommitTool(), PushTool(),
		PullRequestTool(), MergeTool(), ResearchFetchTool(),
		KnowledgeRetrievalTool(nil),
	} {
		expected, named := want[tool.Name]
		if !named {
			t.Errorf("tool %q is unclassified: a new tool must be JUDGED for concurrency, not defaulted", tool.Name)
			continue
		}
		if tool.ParallelSafe != expected {
			t.Errorf("tool %q: ParallelSafe = %v, want %v", tool.Name, tool.ParallelSafe, expected)
		}
	}
}

// TestParallelSafeDefaultsToFalse — the zero value must be the safe one, so a tool added by someone
// who never read any of this serialises instead of fanning out.
func TestParallelSafeDefaultsToFalse(t *testing.T) {
	var tool toolbroker.Tool
	if tool.ParallelSafe {
		t.Fatal("the zero value is parallel-safe; an unconsidered tool would fan out")
	}
}
