package stablerelease

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The SH-3 POSTURE REPORT — this gate's actual output (plan §T10).
//
// It is not a pass mark and it is deliberately not shaped like one. §64.15 asks thirteen-odd things of a
// stable release; two of them are managed-scope and close "not-claimed", the rest close at the level the
// evidence actually reaches, and 188 Appendix-A ids do not collapse to one bit. Every number below is
// RECOMPUTED at print time from the committed bundles, the case corpus and the RC triage table — the report
// is a rendering of the gate, never a document someone maintains alongside it.

// TestPrintSH3PostureReport prints the report. It is a test rather than a command because everything it
// prints is already a function the verifier exports, and a second binary would be a second thing to keep in
// step. `scripts/uat/stable-release` runs it with -v and shows the block.
func TestPrintSH3PostureReport(t *testing.T) {
	report := sh3PostureReport(t)
	t.Log("\n" + report)

	// The report is also written to the bundle directory so an operator who reads the evidence meets it
	// there, next to the manifest it describes, rather than only in a terminal that has scrolled away.
	if os.Getenv("PALAI_WRITE_STABLE_RELEASE_BUNDLE") != "" {
		path := filepath.Join(bundleDir(t), "sh3-posture.md")
		if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
			t.Fatalf("write the posture report: %v", err)
		}
		t.Logf("wrote %s", path)
	}
}

// TestCommittedPostureReportIsCurrent keeps the committed report equal to the recompute. A posture report
// that could drift from the gate would be the most quotable stale document in the repository.
func TestCommittedPostureReportIsCurrent(t *testing.T) {
	path := filepath.Join(bundleDir(t), "sh3-posture.md")
	committed, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed posture report: %v", err)
	}
	if got, want := string(committed), sh3PostureReport(t); got != want {
		t.Fatalf("%s has drifted from the recompute — regenerate with PALAI_WRITE_STABLE_RELEASE_BUNDLE=1 "+
			"go test ./tests/uat/stable-release/ (committed %d bytes, recomputed %d)", path, len(got), len(want))
	}
}

func sh3PostureReport(t *testing.T) string {
	t.Helper()
	index, err := uat.RecomputeReleaseIndex()
	if err != nil {
		t.Fatalf("recompute the release index: %v", err)
	}
	checklist := uat.RecomputeStableChecklist(index)
	blockers, err := uat.RecomputeRCBlockers()
	if err != nil {
		t.Fatalf("read the RC-blocker count: %v", err)
	}
	tiers, err := uat.AggregateCapabilityTiers()
	if err != nil {
		t.Fatalf("recompute the product-wide posture: %v", err)
	}

	var b strings.Builder
	p := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	p("# SH-3 POSTURE — %s", uat.StableReleaseBundle)
	p("")
	p("**This is a release CANDIDATE, not a stable release.** Every line below is RECOMPUTED from the")
	p("committed evidence bundles, the materialized case corpus and the RC triage table by")
	p("`TestPrintSH3PostureReport`; nothing here is maintained by hand. SH-3 Stable is the operator")
	p("attestation `StableReleasePromoteGate` demands — it names all %d §6 operator legs one by one, and NOT",
		len(uat.StableAttestationLegs))
	p("ONE of them has been executed in this program.")
	p("")

	p("## Appendix-A UAT index")
	p("")
	counts := map[string]int{}
	for _, e := range index {
		counts[e.Disposition]++
	}
	p("| Disposition | Count | Means |")
	p("|---|---:|---|")
	p("| `bundle-carried` | %d | a committed evidence bundle carries the case with an outcome |", counts[uat.DispositionBundleCarried])
	p("| `case-materialized` | %d | `tests/uat/cases/<ID>/case.yaml` exists and the catalog gates resolve its in-tree proofs; no bundle carries it |", counts[uat.DispositionCaseMaterialized])
	p("| `managed-scope` | %d | outside this program by decision (master plan §2.2/§9), not by omission |", counts[uat.DispositionManagedScope])
	p("| `unmaterialized` | %d | no bundle, no case directory. Stated because silence would be the dishonest answer |", counts[uat.DispositionUnmaterialized])
	p("| **total** | **%d** | every exact id in master plan Appendix A |", len(index))
	p("")
	notPass := 0
	for _, e := range index {
		if e.Disposition == uat.DispositionBundleCarried && e.Outcome != "PASS" {
			notPass++
		}
	}
	p("Bundle-carried cases not reporting PASS: **%d**.", notPass)
	p("")
	p("The managed-scope ids and why:")
	p("")
	managed := make([]string, 0, len(uat.ManagedScopeUATIDs))
	for id := range uat.ManagedScopeUATIDs {
		managed = append(managed, id)
	}
	sort.Strings(managed)
	for _, id := range managed {
		p("- `%s` — %s", id, uat.ManagedScopeUATIDs[id])
	}
	p("")

	p("## §64.15 stable-release checklist")
	p("")
	p("| Status | Item |")
	p("|---|---|")
	for _, item := range checklist {
		p("| `%s` | %s |", item.Status, item.Item)
	}
	p("")
	p("Statuses: `evidenced` = every claim is bundle-carried and PASS. `proven-not-bundled` = every claim is")
	p("at least a materialized case, but some carry no bundle evidence. `incomplete` = at least one claim is")
	p("unmaterialized or not PASS. `not-claimed` = managed-scope.")
	p("")
	for _, item := range checklist {
		if len(item.Missing) == 0 {
			continue
		}
		p("**%s** — outstanding: %s", item.Item, strings.Join(item.Missing, "; "))
		p("")
	}

	p("## Product-wide capability posture")
	p("")
	p("Recomputed from EVERY committed bundle's claim outcomes and asserted bit-equal to the fully-mounted")
	p("router's `/v1/capabilities` (`TestServedCapabilityTiersEqualTheAggregateRecompute`). **No shipped")
	p("deployment config sets `PALAI_CAPABILITY_WORKER_LISTEN_ADDR`, so no deployed binary serves this exact")
	p("map** — EXT-1 in the RC triage, and the bundle's proof declares it.")
	p("")
	p("| Capability | Tier |")
	p("|---|---|")
	for _, capability := range uat.CapabilityTierOrder {
		p("| `%s` | %s |", capability, tiers[capability])
	}
	p("")

	p("## Zero open P0/P1")
	p("")
	p("`RC-BLOCKERS: %d`, read mechanically from `docs/operations/known-gaps-1.0.md` at verification time.", blockers)
	p("A non-zero count REFUSES this gate. Zero blockers is not zero risk: read `SUP-2` (fail-closed")
	p("comparisons ship defeated) and `AUD-1` (a retention purge reads as tampering) before treating it as an")
	p("all-clear.")
	p("")

	p("## What this gate does NOT claim")
	p("")
	for _, leg := range uat.StableAttestationLegs {
		p("- **%s** — %s", leg.ID, leg.Leg)
	}
	p("")
	p("Each is refused BY NAME by `StableReleasePromoteGate` when a `stable` promote's attestation omits it")
	p("(`TestStablePromoteRefusesAnAttestationMissingALeg` drives all %d in turn). The local closure of this",
		len(uat.StableAttestationLegs))
	p("gate is an RC.")
	p("")
	p("END POSTURE")
	return b.String()
}
