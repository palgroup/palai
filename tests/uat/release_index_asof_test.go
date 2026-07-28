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

// TestTheAsOfRuleIsABitExactNoOpOverTheCommittedCorpus is what makes the rule a FIX rather than a rewrite of
// history: over the bundles committed today it changes not one row, so no shipped bundle's anchor moved when
// it landed. If a future edit makes the dated recompute disagree with the undated one, that is a real change
// to what the RC bundle says evidenced it and it must be argued, not discovered.
func TestTheAsOfRuleIsABitExactNoOpOverTheCommittedCorpus(t *testing.T) {
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
	// "Undated" is the OLD behaviour, reproduced by an as-of far enough in the future to include everything.
	undated, err := recomputeReleaseIndexFrom(carriers, "9999-12-31T23:59:59Z")
	if err != nil {
		t.Fatalf("undated recompute: %v", err)
	}
	if len(dated) != len(undated) {
		t.Fatalf("the two recomputes returned %d and %d entries", len(dated), len(undated))
	}
	for i := range dated {
		if dated[i] != undated[i] {
			t.Errorf("%s: dated recompute says %+v, undated says %+v — the as-of rule was introduced as a no-op over this corpus, so a difference here is a change to what a SHIPPED release says evidenced it",
				dated[i].ID, dated[i], undated[i])
		}
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
