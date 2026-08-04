package relay

import (
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// TestEveryCanonicalToolHasAHumanTitle binds the two lists that would otherwise drift apart. The
// relay's fallback titles an unmapped tool with its RAW NAME, so a tool added to the canonical set
// with no phrase here does not fail — it degrades, and it degrades on the surface a human reads.
// This is the "no Slack copy that can disagree" requirement, asserted rather than trusted.
func TestEveryCanonicalToolHasAHumanTitle(t *testing.T) {
	for _, tool := range toolset.All() {
		title, ok := toolTitles[tool]
		if !ok {
			t.Errorf("%q is granted by a bring-up but has no title: a Slack card will show the raw name", tool)
			continue
		}
		if title == "" {
			t.Errorf("%q maps to an empty title", tool)
		}
		if title == tool {
			t.Errorf("%q maps to itself, which is what the fallback already does", tool)
		}
	}
}

// knownNonGrantTitles are the titles that legitimately sit outside the canonical grant set, each one
// NAMED rather than waved through by a blanket rule. palai.slack.search is Slack-specific and resolves
// through the Slack lookup path (execution/tools/slack_search.go:150, a broker lookup rather than a
// static registration, deliberately); palai.task and palai.todo are defined
// (execution/tools/task.go:14,18) but have no caller and are not in toolbroker.New; palai.web.search
// and palai.fs.write are titled here and registered nowhere.
//
// THE DAY ONE OF THESE IS REGISTERED AND GRANTED it moves to the canonical list and comes out of this
// map — the map shrinking is the signal that the surface grew.
var knownNonGrantTitles = map[string]bool{
	"palai.slack.search": true,
	"palai.task":         true,
	"palai.todo":         true,
	"palai.web.search":   true,
	"palai.fs.write":     true,
}

// TestNoTitleIsOrphaned sweeps the OTHER direction, and the direction is the whole point: a walk over
// the canonical list finds tools with no title, and ONLY this walk finds a title left behind by a tool
// that was renamed or removed. One direction cannot find both.
func TestNoTitleIsOrphaned(t *testing.T) {
	canonical := map[string]bool{}
	for _, tool := range toolset.All() {
		canonical[tool] = true
	}
	for tool := range toolTitles {
		if canonical[tool] || knownNonGrantTitles[tool] {
			continue
		}
		t.Errorf("toolTitles carries %q, which is neither granted nor a named exception: a renamed or "+
			"removed tool leaves its title behind, and this is the only check that sees it", tool)
	}
}
