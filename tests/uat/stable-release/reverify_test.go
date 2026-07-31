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
	// E28's family is checked FIRST of all in PromoteGateFor, one level above E26, and this bundle inherits
	// the dispatch hazard one layer deeper again: it carries the E26 background claim, the E25 admin-console
	// claim, the E23 tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim, the E20
	// agent-surface claim, the E19 wiring claim AND E17 area claims, because it derives its inherited case
	// set from those releases. Dispatching on any of them would reroute it to a gate that knows nothing about
	// the pool birth path, the waiting room admitted from a screen, the minted-value scan, the approver-list
	// equality, the axe coverage, the confirmation split or the ceiling ids. It is recognized by the E28 CASE
	// IDS — and their `FLC-` prefix is part of that decision in BOTH directions, exactly as `BGT-`, `CON-`,
	// `FLT-`, `HIL-` and `CAS-` were.
	//
	// AND THIS TABLE IS THE FIFTH REGISTRATION POINT, WHICH E28's OWN PLAN §1.5 UNDERCOUNTED. That section
	// names five (committedBundleSurfaces, the caseChecksumParts branch, extensionIDPrefixes, the
	// PromoteGateFor dispatch, and the E17 catalog's owner list) and calls the list complete after correcting
	// the memory that said two. It is SIX: this map is the one that fails LOUDLY for a new bundle — "in
	// neither promoteFamilies nor unfamiliedBundles" — and it is the only place where "which gate judges this
	// release" is written down as a decision rather than inferred from a dispatch. The correction is recorded
	// here rather than only in a report, because the next plan will copy the number from this one.
	"fleet-console-0.1.0": "E28 fleet-console (the pool birth path re-derived from the pool ledger with the POSTURE SET required to contain `unsandboxed-host` — the value no code path in this tree could write before T1, so a pool COUNT alone would have been satisfied by the seed every tenant gets at birth + a machine that actually reached `pending` in a strict pool and at least one ADMITTED FROM THE CONSOLE, by value, because an epic whose crown claim is a screen cannot certify it from a CLI transcript + ZERO minted key values across five sites each DECODED before it was scanned and each naming a harmless token the same scan DID find, the fifth being a LATER response that the other four pass over + the approver-entry EQUALITY across a policy write, over requests carrying all five fields, because a stored-outcome assertion alone passes on a server that merged + the declared and axe-scanned routes re-derived from lib/routes.ts and EQUAL in both colour schemes + the confirmation split refused in BOTH directions over a source sweep of every page, since a console that guards everything has stopped distinguishing rather than become more careful + the four ceiling gap ids the screens state, `FLT-P15` among them + the composed background/admin-console/tool-approval/code-and-ship/tools-memory/agent-surface/wiring/extensions/eval gates)",
	// E26's family is checked next in PromoteGateFor, one level above E25, and this bundle inherits
	// the dispatch hazard one layer deeper again: it carries the E25 admin-console claim, the E23
	// tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim, the E20 agent-surface
	// claim, the E19 wiring claim AND E17 area claims, because it derives its inherited case set from those
	// releases. Dispatching on any of them would reroute it to a gate that knows nothing about the six
	// replicated semantics, the refusal controls, the two ownership postures, the exactly-once notice, the
	// reaper's duties or the redaction sites. It is recognized by the E26 CASE IDS — and their `BGT-` prefix
	// is part of that decision in BOTH directions, exactly as `CON-`, `FLT-`, `HIL-` and `CAS-` were.
	//
	// AND THE COMMENT BELOW UNDERCOUNTED, WHICH THIS ENTRY CORRECTS RATHER THAN REPEATS. Every earlier copy
	// of it calls this "the THIRD registration point a new bundle name owes" and names two others
	// (committedBundleSurfaces and the caseChecksumParts branch). There is a FOURTH and it is the one that
	// decides the outcome: the `for _, c := range m.Cases` clause in PromoteGateFor itself. Without that
	// clause a bundle registered in all three of the others still routes to a WEAKER family gate and PASSES —
	// which is the promote-gate-family-dispatch defect this tree has already shipped once, and it is not
	// caught by the sweep below, because the sweep asks whether SOME gate passed the bundle rather than
	// whether the RIGHT one did.
	"background-execution-0.1.0": "E26 background-execution (the six §2 semantics re-derived from the semantics ledger with §2.6 — the model calling another tool while the process still ran — required by name + ZERO processes started under any refusal over refusals that each carry a non-vacuity control with its unit stated + both sandbox postures outliving the call that started them and ZERO signals to a handle we could not prove was ours + exactly one notice per settled task across two ticks / two control planes / a restart / a running run / a terminal run, each naming the mutation that reddens it + the reaper's six duties, none read off our own bookkeeping + ZERO environment values in five landing sites, each DECODED before it was scanned + the composed admin-console/tool-approval/code-and-ship/tools-memory/agent-surface/wiring/extensions/eval gates)",
	// E24's family is checked FIRST of all in PromoteGateFor, one level above E23, and this bundle inherits
	// the dispatch hazard one layer deeper again: it carries the E23 tool-approval claim, the E22
	// code-and-ship claim, the E21 tools-memory claim, the E20 agent-surface claim, the E19 wiring claim AND
	// E17 area claims, because it derives its inherited case set from those releases. Dispatching on any of
	// them would reroute it to a gate that knows nothing about the wrong-pool sweep, the cross-tenant sweep,
	// the capacity-death count, the key-revocation fence or the server-mint recompute. It is recognized by
	// the E24 CASE IDS — and their `FLT-` prefix is part of that decision in BOTH directions, exactly as
	// `HIL-` and `CAS-` were.
	//
	// THIS ENTRY IS THE THIRD REGISTRATION POINT A NEW BUNDLE NAME OWES (the other two are
	// committedBundleSurfaces and the caseChecksumParts branch, both in tests/uat/evidence.go). Without it
	// the sweep below reports "in neither promoteFamilies nor unfamiliedBundles" — deliberately, because a
	// bundle that lost its family would otherwise move from "passes its own gate" to "no gate recognizes it"
	// in silence.
	"runner-fleet-0.1.0":  "E24 runner-fleet (the wrong-pool and cross-tenant offer sweeps over the offer ledger + the capacity-death count over the run ledger + the key-revocation fence that COUNTS the renewals after the revocation and requires all of them to have succeeded + the machine-revocation survival sweep across two gateway generations + the server-mint recompute over the registry ledger including a label two identities came in under + the composed tool-approval/code-and-ship/tools-memory/agent-surface/wiring/extensions/eval gates)",
	"admin-console-0.1.0": "E25 admin-console (the relay-gate totality re-derived from every exported HTTP method beside the identity gate it opens with + the axe coverage equality over every route lib/routes.ts declares, in BOTH colour schemes + the ciphertext query pin over the whole storage/queries corpus + the byte scan of the DOM, every response body and every browser-served source map, each layer naming a probe it found + the conformance sweep's risen item floor + the FIVE divergence-ledger rows measured wrong and re-observed + the shipped runbook on /v1 alone + the approvals decided from a screen with none applied on a mismatched hash + the composed tool-approval/code-and-ship/tools-memory/agent-surface/wiring/extensions/eval gates)",
	// E23's family is checked next in PromoteGateFor, one level above E22, and this bundle inherits
	// the same dispatch hazard one layer deeper: it carries the E22 code-and-ship claim, the E21
	// tools-memory claim, the E20 agent-surface claim, the E19 wiring claim AND E17 area claims, because it
	// derives its inherited case set from those releases. Dispatching on any of them would reroute it to a
	// gate that knows nothing about the ungoverned-side-effect sweep, the screen-authorship fence, the
	// park/expiry counts or the single-mint recompute. It is recognized by the E23 CASE IDS — and their
	// `HIL-` prefix is part of that decision in BOTH directions, exactly as `CAS-` was.
	//
	// THIS ENTRY IS THE THIRD REGISTRATION POINT A NEW BUNDLE NAME OWES (the other two are
	// committedBundleSurfaces and the caseChecksumParts branch, both in tests/uat/evidence.go). Without it
	// the sweep below reports "in neither promoteFamilies nor unfamiliedBundles" — deliberately, because a
	// bundle that lost its family would otherwise move from "passes its own gate" to "no gate recognizes it"
	// in silence.
	"tool-approval-0.1.0": "E23 tool-approval (the ungoverned-side-effect sweep over the call ledger + the screen-authorship fence swept in both directions + the park/expiry counts over the run ledger + the unauthorized-decision sweep across both surfaces + the single-mint recompute over two packages' source + the composed code-and-ship/tools-memory/agent-surface/wiring/extensions/eval gates)",
	// E22's family is checked next in PromoteGateFor, one level above E21, and this bundle is the
	// hardest in the tree to dispatch correctly: it carries the E21 tools-memory claim, the E20 agent-surface
	// claim, the E19 wiring claim AND E17 area claims, because it derives its inherited case set from those
	// releases. Dispatching on any of them would reroute it to a gate that knows nothing about the
	// unapproved-publication sweep, the destination sweep or the typed-operation ceiling. It is recognized by
	// the E22 CASE IDS — and their `CAS-` prefix is part of that decision in BOTH directions: an id already
	// inside AgentSurfaceCaseIDs or ToolsMemoryCaseIDs would have matched an EARLIER family marker, and an
	// `SLK-` id outside both would have regenerated a shipped bundle.
	"code-and-ship-0.1.0": "E22 code-and-ship (the unapproved-publication sweep over the ledger + the destination sweep over the two publish tools' schemas + the typed-operation ceiling recomputed from workers/catalog.go's source + the composed tools-memory/agent-surface/wiring/extensions/eval gates)",
	// E21's family is checked next, one level above E20 and for the same reason a
	// level down: a tools-and-memory bundle also carries the E20 agent-surface claim (it derives its
	// inherited case set from that release), so dispatching on that would reroute it to a gate that knows
	// nothing about the stored-search-byte or mention re-derivations. It is recognized by the E21 CASE IDS —
	// and their `TLM-` prefix is part of that decision: an `SLK-` id outside AgentSurfaceCaseIDs would have
	// matched NO family marker and fallen through to a weaker gate entirely.
	"tools-memory-0.1.0": "E21 tools-and-memory (the stored-search-byte sweep over the persisted surface + the mention re-derivation over the answer's own bytes + the composed agent-surface/wiring/extensions/eval gates)",
	// E20's family is checked next, for the reason E19's clause below describes
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
