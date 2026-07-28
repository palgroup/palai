package uat

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// THE RELEASE INDEX IS DATED, AND THIS IS THE GUARD THAT KEEPS IT THAT WAY.
//
// The defect these two tests close was MEASURED on 2026-07-28 rather than imagined: committing a bundle
// named `code-and-ship-0.1.0` — which sorts between `automation-0.1.0` and `coding-0.1.0` — displaced
// `extensions-0.1.0` as the indexed carrier of thirty Appendix-A ids and made all eight of the SHIPPED
// release-1.0.0-rc1 bundle's checksums stop recomputing. Nothing was wrong with the RC; a release cut two
// days later had rewritten the anchor it was hashed against.
//
// Three epics before this one escaped the same trap by naming luck (`integration-wiring-`,
// `slack-agent-surface-` and `tools-memory-` all sort after the bundles whose case sets they inherit), which
// is exactly the kind of green that stops being green on the first name that does not.

// TestABundleCapturedAfterTheIndexCannotDisplaceItsCarrier drives the pure core with a synthetic corpus, and
// it asserts BOTH directions — a rule that only ever refuses is indistinguishable from a rule that refuses
// everything.
func TestABundleCapturedAfterTheIndexCannotDisplaceItsCarrier(t *testing.T) {
	const indexed = "2026-07-26T11:00:00Z"
	id := AppendixAUATIDs[0]

	carriers := map[string][]bundleCarrier{
		id: {
			{Release: "extensions-0.1.0", Status: "PASS", CapturedAt: "2026-07-25T00:00:00Z"},
			// Sorts BEFORE extensions-0.1.0 by name, and was captured two days after the index.
			{Release: "code-and-ship-0.1.0", Status: "PASS", CapturedAt: "2026-07-28T00:00:00Z"},
		},
	}
	index, err := recomputeReleaseIndexFrom(carriers, indexed)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	got := entryFor(t, index, id)
	if got.Bundle != "extensions-0.1.0" {
		t.Errorf("%s is indexed to %q — a bundle captured AFTER this index cannot be what evidenced it, and letting it win rewrites a shipped release's anchor", id, got.Bundle)
	}

	// The discriminating half: the same carrier, captured BEFORE the index, must win. Without it this test
	// would pass over an implementation that ignored every carrier whose name sorted early.
	carriers[id][1].CapturedAt = "2026-07-24T00:00:00Z"
	index, err = recomputeReleaseIndexFrom(carriers, indexed)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if got := entryFor(t, index, id); got.Bundle != "code-and-ship-0.1.0" {
		t.Errorf("%s is indexed to %q — a bundle that existed when the index was captured and sorts first IS its carrier; the as-of rule must date carriers, not blanket-drop them", id, got.Bundle)
	}

	// And the pessimistic rule still outranks the ordering: a non-PASS carrier wins whatever its name.
	carriers[id][0].Status = "FAIL"
	index, err = recomputeReleaseIndexFrom(carriers, indexed)
	if err != nil {
		t.Fatalf("recompute: %v", err)
	}
	if got := entryFor(t, index, id); got.Outcome != "FAIL" {
		t.Errorf("%s is indexed as %q — the FIRST NON-PASS carrier wins, or an id that failed in one bundle is laundered by a green copy in another", id, got.Outcome)
	}

	if _, err := recomputeReleaseIndexFrom(carriers, ""); err == nil {
		t.Error("an index with no as-of date recomputed anyway — undated, every future bundle is indistinguishable from a past one, so this must fail CLOSED")
	}
}

// TestTheAsOfRuleIsWhatKeepsTheShippedRCGreen is the rule's load-bearing half, and it reads as the record of
// what was measured. When the rule landed it was a bit-exact no-op: the three bundles committed after the RC
// (`integration-wiring-`, `slack-agent-surface-`, `tools-memory-`) all sort AFTER the bundles whose case sets
// they inherit, so none of them ever won a carrier row. `code-and-ship-0.1.0` sorts between
// `automation-0.1.0` and `coding-0.1.0`, and it does.
//
// So this asserts both directions:
//
//   - the DATED recompute — the shipped behaviour — reproduces the anchor the RC bundle's committed checksums
//     were taken against, which is the whole point;
//   - the UNDATED recompute — the OLD behaviour, reproduced by an as-of far enough in the future to include
//     everything — DIFFERS. A rule whose removal changed nothing would be decoration, and this one is the
//     only thing standing between a new bundle's name and eight red checksums in a shipped release.
func TestTheAsOfRuleIsWhatKeepsTheShippedRCGreen(t *testing.T) {
	carriers, err := CommittedBundleOutcomes()
	if err != nil {
		t.Fatalf("gather the committed bundle outcomes: %v", err)
	}
	asOf, err := releaseIndexAsOf()
	if err != nil {
		t.Fatalf("date the index: %v", err)
	}
	dated, err := recomputeReleaseIndexFrom(carriers, asOf)
	if err != nil {
		t.Fatalf("dated recompute: %v", err)
	}
	undated, err := recomputeReleaseIndexFrom(carriers, "9999-12-31T23:59:59Z")
	if err != nil {
		t.Fatalf("undated recompute: %v", err)
	}

	// The RC bundle carries the index it was hashed against. The DATED recompute must reproduce its anchor.
	raw, err := os.ReadFile(filepath.Join(releasesDir(), StableReleaseBundle, "manifest.json"))
	if err != nil {
		t.Fatalf("read the RC bundle: %v", err)
	}
	var rc struct {
		Cases []struct {
			Proof *struct {
				IndexAnchor string `json:"index_anchor"`
			} `json:"release_index_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &rc); err != nil {
		t.Fatalf("decode the RC bundle: %v", err)
	}
	committed := ""
	for _, c := range rc.Cases {
		if c.Proof != nil && c.Proof.IndexAnchor != "" {
			committed = c.Proof.IndexAnchor
			break
		}
	}
	if committed == "" {
		t.Fatal("the RC bundle carries no release_index_proof anchor — there is nothing for the dated recompute to reproduce")
	}
	if got := hashPartsOfIndex(dated); got != committed {
		t.Errorf("the DATED recompute anchors at %s but the shipped RC bundle was hashed against %s — a release cut after the RC has moved what the RC says evidenced it", got, committed)
	}

	// And the removal test: without the rule the anchor moves, which is exactly the defect that was measured.
	moved := 0
	for i := range dated {
		if i < len(undated) && dated[i] != undated[i] {
			moved++
		}
	}
	if moved == 0 {
		t.Errorf("dropping the as-of rule changes NO index row over today's corpus, so this test no longer demonstrates the rule is load-bearing — that is not automatically wrong (it was true when the rule landed), but it must be re-argued rather than assumed: a bundle named earlier than %q would silently move %d rows the moment it is committed", "coding-0.1.0", len(dated))
	} else if hashPartsOfIndex(undated) == committed {
		t.Error("the UNDATED recompute still reproduces the RC's anchor although rows moved — the anchor is not sensitive to the rows, which would make it a weaker anchor than it claims to be")
	} else {
		t.Logf("the as-of rule is load-bearing: without it %d of %d index rows move and the shipped RC's %d committed checksums stop recomputing", moved, len(dated), len(rc.Cases))
	}
}

// TestEveryCommittedBundleCarriesACaptureDate is the fail-closed half: the as-of comparison is meaningless
// for a bundle with no date, and defaulting an undated bundle to "before the index" is how the rule would be
// pierced by omission.
func TestEveryCommittedBundleCarriesACaptureDate(t *testing.T) {
	entries, err := os.ReadDir(releasesDir())
	if err != nil {
		t.Fatalf("read evidence/releases: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(releasesDir(), e.Name(), "manifest.json"))
		if err != nil {
			t.Errorf("read %s: %v", e.Name(), err)
			continue
		}
		var m evidenceManifest
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Errorf("decode %s: %v", e.Name(), err)
			continue
		}
		if m.CapturedAt == "" {
			t.Errorf("%s carries no captured_at — the release index cannot tell whether it predates the release it would appear in", e.Name())
		}
	}
}

func entryFor(t *testing.T, index []ReleaseIndexEntry, id string) ReleaseIndexEntry {
	t.Helper()
	for _, e := range index {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("%s is not in the recomputed index", id)
	return ReleaseIndexEntry{}
}
