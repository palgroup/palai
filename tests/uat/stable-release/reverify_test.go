package stablerelease

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE CROSS-EPIC RE-VERIFY (plan §T10): every previous epic's bundle re-verified through the STRENGTHENED
// verifier, each with 0 findings, and every bundle that has a promote FAMILY re-judged by its OWN gate.
//
// The second half is the part a summary would skip. Re-verifying a manifest proves its fields are
// well-formed and its checksums recompute; running its own promote gate proves the release-family rule that
// epic closed on — E15's rollback+restore, E16's cross-language parity + gateway-off, E17's capability tier
// table + eval thresholds — still holds at this commit, against today's code rather than that epic's.

// promoteFamilies is the CODE-side declaration of which committed bundle is judged by which promote family,
// and — for the bundles that have none — why. It exists so "this bundle has no gate" is a recorded decision
// rather than a silence: without it, a bundle that LOST its family claims would quietly move from "passes
// its own gate" to "no gate recognizes it" and the sweep below would still be green.
var promoteFamilies = map[string]string{
	// E20's family is checked FIRST of all in PromoteGateFor, for the reason E19's clause below describes
	// one level down: an agent-surface bundle also carries the E19 wiring claim (it derives its inherited
	// case set from that release), so dispatching on that would reroute it to a gate that knows nothing
	// about the forgery derivation. It is recognized by the E20 CASE IDS rather than by the
	// agent_surface_claim it enforces — dispatching on the claim a gate enforces is how a release drops it.
	"slack-agent-surface-0.1.0": "E20 agent-surface (the forgery re-derivation over the closing blocks + the three admission entrances + the composed wiring/extensions/eval gates)",
	// E19's family is checked AHEAD of E18's in PromoteGateFor: a wiring bundle also carries E17 area
	// claims, so dispatching on those would reroute it to a gate that knows nothing about mounts.
	"integration-wiring-0.1.0":  "E19 wiring (mount derivation from the running stack + no-tier-advance against the E17 baseline + the composed extensions gate)",
	"release-1.0.0-rc1":         "E18 stable-release (release index + product-wide posture + SUP-3's verified artifact set)",
	"extensions-0.1.0":          "E17 extensions (capability tier table + QUA-003 precondition + the eval gate)",
	"sdk-provider-parity-0.1.0": "E16 SDK parity (three-language equality + gateway-off)",
	"self-host-0.2.0":           "E15 upgrade (rollback + restore/DR proof)",
}

// unfamiliedBundles are the committed bundles NO promote gate recognizes, with the reason. Every one predates
// the promote-gate machinery (E15 T6): they are journey evidence for a milestone, not a release candidate,
// and PromoteGateFor is expected to refuse them with "no promote policy for this release".
var unfamiliedBundles = map[string]string{
	"automation-0.1.0":               "E11 journey evidence; the promote gate machinery is E15 T6",
	"coding-0.1.0":                   "E09/E10 journey evidence",
	"extensibility-0.1.0":            "E12 journey evidence",
	"interactive-0.1.0":              "E08 journey evidence",
	"local-live-0.1.0":               "LP-0 journey evidence",
	"local-live-0.1.0-chaining":      "LP-0 journey evidence",
	"local-live-0.1.0-command-spine": "LP-0 journey evidence",
	"local-live-0.1.0-config-switch": "LP-0 journey evidence",
	"local-live-0.1.0-lifecycle":     "LP-0 journey evidence",
	"local-live-0.1.0-subagents":     "LP-0 journey evidence",
	"managed-cloud-0.1.0":            "E13 journey evidence; MCI claims carry no release-family rule",
	"recovery-0.1.0":                 "E10 journey evidence",
	"self-host-0.1.0":                "SH-0 alpha evidence; the rollback+restore family arrives with self-host-0.2.0",
}

// TestEveryCommittedBundleReVerifiesClean is the cross-epic re-verify. It reads the directory rather than a
// list so a bundle cannot be dropped from the sweep by being forgotten.
func TestEveryCommittedBundleReVerifiesClean(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "evidence", "releases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence/releases: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		seen++
		summary, err := uat.VerifyRelease(filepath.Join(dir, e.Name()), nil)
		if err != nil {
			t.Errorf("%s: %v", e.Name(), err)
			continue
		}
		if !summary.OK() {
			t.Errorf("%s did NOT re-verify clean through the strengthened verifier: %s\n%v",
				e.Name(), summary, summary.Findings)
			continue
		}
		t.Logf("RE-VERIFIED %-34s %s", e.Name(), summary)
	}
	if seen < len(promoteFamilies)+len(unfamiliedBundles) {
		t.Fatalf("only %d bundles were re-verified but the tables below account for %d — the corpus shrank",
			seen, len(promoteFamilies)+len(unfamiliedBundles))
	}
}

// TestEveryBundleWithAPromoteFamilyPassesItsOwnGate re-judges each bundle by the gate its own epic closed on.
// A bundle without a family must be REFUSED with the no-policy message — silence would let a release that
// lost its family claims look identical to one that never had any.
func TestEveryBundleWithAPromoteFamilyPassesItsOwnGate(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "evidence", "releases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence/releases: %v", err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, release := range names {
		raw, err := os.ReadFile(filepath.Join(dir, release, "manifest.json"))
		if err != nil {
			t.Errorf("%s: %v", release, err)
			continue
		}
		refusals := uat.PromoteGateFor(raw, "rc")
		family, hasFamily := promoteFamilies[release]
		why, unfamilied := unfamiliedBundles[release]

		switch {
		case hasFamily && unfamilied:
			t.Errorf("%s is in BOTH tables — decide which", release)
		case hasFamily:
			if len(refusals) != 0 {
				t.Errorf("%s (%s) was REFUSED by its own promote gate at this commit: %v", release, family, refusals)
				continue
			}
			t.Logf("PROMOTE-rc PASS %-34s %s", release, family)
		case unfamilied:
			if len(refusals) == 0 {
				t.Errorf("%s has no declared promote family but PASSED a promote — a release no gate recognizes must be refused, not blessed", release)
				continue
			}
			if !refusalsMention(refusals, "no promote policy for this release") {
				t.Errorf("%s was refused, but not as an unfamilied release (it may have GAINED claims a gate recognizes — update promoteFamilies): %v", release, refusals)
				continue
			}
			t.Logf("PROMOTE-rc REFUSED (no policy, as declared) %-34s %s", release, why)
		default:
			t.Errorf("%s is in neither promoteFamilies nor unfamiliedBundles — a new bundle must declare which gate judges it, or none and why", release)
		}
	}

	for release := range promoteFamilies {
		if !contains(names, release) {
			t.Errorf("%s is in promoteFamilies but is not committed", release)
		}
	}
	for release := range unfamiliedBundles {
		if !contains(names, release) {
			t.Errorf("%s is in unfamiliedBundles but is not committed", release)
		}
	}
}

func contains(all []string, want string) bool {
	for _, s := range all {
		if s == want {
			return true
		}
	}
	return false
}

// TestNoBundleNamesItselfStable is the honest-naming guard at the corpus level. Nothing in this repository
// may ship a bundle whose NAME claims a stable release: the stable flip is the operator's attested act.
func TestNoBundleNamesItselfStable(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "evidence", "releases")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read evidence/releases: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() && strings.Contains(strings.ToLower(e.Name()), "stable") {
			t.Errorf("evidence/releases/%s names itself stable — SH-3 Stable is the operator attestation the promote gate demands, and no local session declares it", e.Name())
		}
	}
}
