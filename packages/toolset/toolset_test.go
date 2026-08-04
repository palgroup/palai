package toolset_test

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

// anthropicDefined names every tool whose identifier belongs to Anthropic rather than to this
// platform. Each entry is a literal the model was trained against; the day one is removed from the
// canonical lists it should come out of here too.
var anthropicDefined = map[string]bool{
	"str_replace_based_edit_tool": true, // text_editor_20250728
}

// TestEveryListIsNonEmptyAndWellFormed pins the shape a grant depends on. A list that is empty, or
// that carries a name in the wrong namespace, empties or corrupts every effective tool set computed
// from it (execution/config.go:120-137 intersects LAST, so garbage in is silence out).
func TestEveryListIsNonEmptyAndWellFormed(t *testing.T) {
	lists := map[string][]string{
		"Default":    toolset.Default(),
		"Repository": toolset.Repository(),
		"Publish":    toolset.Publish(),
	}
	for name, list := range lists {
		if len(list) == 0 {
			t.Fatalf("%s() is empty: an empty baseline empties every effective tool set", name)
		}
		seen := map[string]bool{}
		for _, tool := range list {
			// ANTHROPIC-DEFINED TOOLS ARE THE ONE EXEMPTION, and it is a named list rather than a
			// loosened rule: their names are literals the model was trained against, so a `palai.`
			// spelling would be a different tool as far as it is concerned. Everything we author still
			// has to live in our namespace, and an unknown unprefixed name still fails.
			if !strings.HasPrefix(tool, "palai.") && !anthropicDefined[tool] {
				t.Errorf("%s() carries %q, which is neither in the palai. namespace nor a named Anthropic-defined tool", name, tool)
			}
			if seen[tool] {
				t.Errorf("%s() lists %q twice", name, tool)
			}
			seen[tool] = true
		}
	}
}

// TestDefaultCarriesNoSlackSpecificTool is the user's explicit requirement, asserted rather than
// assumed: the default set is the GENERIC platform surface. palai.slack.search is not in the
// broker's static set either, so granting it would offer a name that cannot resolve.
func TestDefaultCarriesNoSlackSpecificTool(t *testing.T) {
	for _, tool := range toolset.Default() {
		if strings.HasPrefix(tool, "palai.slack.") {
			t.Errorf("Default() carries the Slack-specific tool %q", tool)
		}
	}
}

// TestAllIsTheUnionAndTheListsAreDisjoint keeps All() honest as the single thing a guard can range
// over. Disjointness matters even though `palai up` writes only Default() today (dated 2026-08-04 —
// no bring-up calls Repository() or Publish(); see toolset.go:27-32): All() is what the resolve and
// title guards range over, and a name shared by two lists would make either guard's per-list bookkeeping
// wrong, plus set up any later grant that DOES combine lists to write a duplicate into the project policy.
func TestAllIsTheUnionAndTheListsAreDisjoint(t *testing.T) {
	seen := map[string]string{}
	for listName, list := range map[string][]string{
		"Default": toolset.Default(), "Repository": toolset.Repository(), "Publish": toolset.Publish(),
	} {
		for _, tool := range list {
			if prior, dup := seen[tool]; dup {
				t.Errorf("%q appears in both %s() and %s()", tool, prior, listName)
			}
			seen[tool] = listName
		}
	}
	all := toolset.All()
	if len(all) != len(seen) {
		t.Fatalf("All() has %d entries, the three lists have %d distinct", len(all), len(seen))
	}
	for _, tool := range all {
		if _, ok := seen[tool]; !ok {
			t.Errorf("All() carries %q, which is in none of the three lists", tool)
		}
	}
}

// TestCallersCannotMutateTheCanonicalLists — a returned slice backed by the package's own array lets
// one caller's append corrupt every later caller's grant.
func TestCallersCannotMutateTheCanonicalLists(t *testing.T) {
	first := toolset.Default()
	if len(first) == 0 {
		t.Fatal("Default() is empty")
	}
	first[0] = "palai.mutated"
	if toolset.Default()[0] == "palai.mutated" {
		t.Fatal("Default() returns its backing array: a caller mutated the canonical list")
	}
}
