package toolset_test

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/toolset"
)

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
			if !strings.HasPrefix(tool, "palai.") {
				t.Errorf("%s() carries %q, which is not in the palai. namespace", name, tool)
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
// over. Disjointness matters because up.go appends Repository()/Publish() onto Default(): an overlap
// would write a duplicate into the project policy.
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
