package stablerelease

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// The E18 T10 PROMOTE GATE — `rc` passes on the local seam, `stable` awaits an operator attestation that
// names every §6 leg one by one.
//
// HONEST CEILING, and the reason every `stable` case below is a REFUSAL: not one §6 operator leg has been
// executed in this program. An attestation written here would be a fabrication, so what is proven is that
// the gate refuses without one — which is the mechanical form of "SH-3 Stable is the operator's attested
// act, not a local declaration".

func refusalsMention(refusals []uat.Refusal, substr string) bool {
	for _, r := range refusals {
		if strings.Contains(r.Detail, substr) {
			return true
		}
	}
	return false
}

func promoteRefusals(t *testing.T, m map[string]any, target string) []uat.Refusal {
	t.Helper()
	return uat.PromoteGateFor(marshal(t, m), target)
}

// TestRCPromotePassesOnTheLocalSeam is the positive: the committed bundle is promotable to `rc`.
func TestRCPromotePassesOnTheLocalSeam(t *testing.T) {
	if refusals := promoteRefusals(t, committedManifest(t), "rc"); len(refusals) != 0 {
		t.Fatalf("the committed RC bundle was REFUSED for rc: %v", refusals)
	}
}

// TestStablePromoteWithNoAttestationIsRefused is the sentence this whole epic closes on.
func TestStablePromoteWithNoAttestationIsRefused(t *testing.T) {
	refusals := promoteRefusals(t, committedManifest(t), "stable")
	if len(refusals) == 0 {
		t.Fatal("a stable promote with NO operator attestation PASSED — SH-3 Stable would be a local declaration")
	}
	if !refusalsMention(refusals, "SH-3 Stable IS that attestation") {
		t.Fatalf("the stable promote was refused, but not on the attestation: %v", refusals)
	}
	t.Logf("REFUSED: %s", refusals[0].Detail)
}

// attestation builds a complete, well-formed stable attestation for the committed release. It is used ONLY
// to prove the gate's refusals are specific — never committed into a bundle, because no leg has run.
func attestation(t *testing.T) map[string]any {
	t.Helper()
	legs := []any{}
	for _, leg := range uat.StableAttestationLegs {
		legs = append(legs, map[string]any{
			"id": leg.ID, "disposition": uat.LegExecuted,
			"detail": "TEST FIXTURE ONLY — this leg has NOT run in this program",
		})
	}
	return map[string]any{
		"schema": uat.StableAttestationSchema, "release": uat.StableReleaseBundle,
		"target": "stable", "attester": "test-fixture", "legs": legs,
	}
}

// TestStablePromoteRefusesAnAttestationMissingALeg is the load-bearing negative: eleven legs, and dropping
// ANY ONE of them refuses the promote by name. Without this, "names every leg" would be a claim about the
// attestation's shape rather than about the gate's behaviour.
func TestStablePromoteRefusesAnAttestationMissingALeg(t *testing.T) {
	for _, drop := range uat.StableAttestationLegs {
		m := committedManifest(t)
		att := attestation(t)
		kept := []any{}
		for _, raw := range att["legs"].([]any) {
			if raw.(map[string]any)["id"] != drop.ID {
				kept = append(kept, raw)
			}
		}
		att["legs"] = kept
		m["operator_attestation"] = att

		refusals := promoteRefusals(t, m, "stable")
		if len(refusals) == 0 {
			t.Fatalf("a stable promote PASSED with §6 leg %s missing from the attestation", drop.ID)
		}
		if !refusalsMention(refusals, "does not name §6 operator leg "+drop.ID) {
			t.Fatalf("dropping leg %s was refused, but not by name: %v", drop.ID, refusals)
		}
	}
	t.Logf("REFUSED a stable promote for each of the %d §6 legs dropped in turn", len(uat.StableAttestationLegs))
}

// TestStablePromoteAttestationRefusalMatrix covers the rest of the record's fences. Each mutation is a way a
// weak attestation could otherwise discharge the strongest gate in the tree.
func TestStablePromoteAttestationRefusalMatrix(t *testing.T) {
	for _, tc := range []struct {
		what   string
		want   string
		mutate func(map[string]any)
	}{
		{"a record of another schema", "is not", func(a map[string]any) { a["schema"] = "palai.release-approval/v1" }},
		{"an attestation bound to a different release", "binds to ONE release", func(a map[string]any) { a["release"] = "self-host-0.2.0" }},
		{"an rc attestation replayed onto a stable promote", "binds to ONE target", func(a map[string]any) { a["target"] = "rc" }},
		{"an unsigned-off attestation", "names no attester", func(a map[string]any) { a["attester"] = "  " }},
		{"a leg attested \"n/a\"", "no \"skipped\"", func(a map[string]any) {
			a["legs"].([]any)[0].(map[string]any)["disposition"] = "n/a"
		}},
		{"a leg with no account of what was run", "is a checkbox, not an attestation", func(a map[string]any) {
			a["legs"].([]any)[0].(map[string]any)["detail"] = ""
		}},
		{"an attestation supplying its own obligation", "may not supply its own obligations", func(a map[string]any) {
			a["legs"] = append(a["legs"].([]any), map[string]any{
				"id": "L-99", "disposition": uat.LegExecuted, "detail": "an obligation the ledger does not have",
			})
		}},
		{"one leg attested twice", "attested twice", func(a map[string]any) {
			legs := a["legs"].([]any)
			a["legs"] = append(legs, legs[0])
		}},
	} {
		m := committedManifest(t)
		att := attestation(t)
		tc.mutate(att)
		m["operator_attestation"] = att
		refusals := promoteRefusals(t, m, "stable")
		if len(refusals) == 0 {
			t.Errorf("a stable promote PASSED with %s", tc.what)
			continue
		}
		if !refusalsMention(refusals, tc.want) {
			t.Errorf("%s was refused, but not for the expected reason (want %q): %v", tc.what, tc.want, refusals)
			continue
		}
		t.Logf("REFUSED %s", tc.what)
	}

	// The control: a COMPLETE (fixture-only) attestation clears the attestation clause, so the negatives
	// above are refusals of what they name rather than of the attestation mechanism as a whole.
	m := committedManifest(t)
	m["operator_attestation"] = attestation(t)
	if refusals := promoteRefusals(t, m, "stable"); len(refusals) != 0 {
		t.Fatalf("a complete attestation was still refused — the negatives above prove nothing: %v", refusals)
	}
}

// TestPromoteRefusesAReleaseWithNoVerifiedArtifactSet is SUP-3's rule firing. T9 wrote: "if T10 does not
// take this rule, no rule enforces it." This is the test that would fail if it were dropped.
func TestPromoteRefusesAReleaseWithNoVerifiedArtifactSet(t *testing.T) {
	m := committedManifest(t)
	// Drop the supply-chain claim entirely — the release still carries its index, its posture and every
	// other proof, and it is still refused, because a release family that verifies no artifacts has
	// nothing to promote.
	c := caseWith(t, m, "supply_chain_claim")
	delete(c, "supply_chain_claim")
	delete(c, "supply_chain_proof")

	refusals := promoteRefusals(t, m, "rc")
	if len(refusals) == 0 {
		t.Fatal("a promote PASSED for a release carrying NO verified artifact set — SUP-3 is enforced nowhere")
	}
	if !refusalsMention(refusals, "must ALWAYS carry a VERIFIED artifact set") {
		t.Fatalf("the promote was refused, but not on SUP-3's rule: %v", refusals)
	}
	t.Logf("REFUSED: %s", refusals[0].Detail)
}

// TestPromoteRefusesAnUnverifiedArtifactSet is the same rule one turn finer: the claim is present but the
// proof names no release directory, which is exactly the `PALAI_RELEASE_DIR` unset case the wrapper lets
// through.
func TestPromoteRefusesAnUnverifiedArtifactSet(t *testing.T) {
	m := committedManifest(t)
	caseWith(t, m, "supply_chain_claim")["supply_chain_proof"].(map[string]any)["release_dir"] = ""
	if !refusalsMention(promoteRefusals(t, m, "rc"), "must ALWAYS carry a VERIFIED artifact set") {
		t.Fatal("a supply-chain proof naming NO release directory was promotable — SUP-3's rule does not bite")
	}
}

// TestDroppingTheIndexClaimDoesNotRerouteToAWeakerGate is the promote-gate family-dispatch rule this
// repository already learned the hard way: never dispatch on the claim the gate enforces. Dropping the
// release index must NOT let this release fall through to the E17/E16/E15 families, which know nothing
// about it and would pass it.
func TestDroppingTheIndexClaimDoesNotRerouteToAWeakerGate(t *testing.T) {
	for _, marker := range []string{"release_index_claim", "aggregate_tier_claim"} {
		m := committedManifest(t)
		c := caseWith(t, m, marker)
		delete(c, marker)
		delete(c, strings.TrimSuffix(marker, "_claim")+"_proof")

		refusals := promoteRefusals(t, m, "rc")
		if len(refusals) == 0 {
			t.Fatalf("dropping %s made this release PROMOTABLE — it rerouted to a weaker family's gate", marker)
		}
		if refusalsMention(refusals, "no promote policy for this release") {
			t.Fatalf("dropping %s left the release with NO gate at all: %v", marker, refusals)
		}
		t.Logf("REFUSED after dropping %s: %s", marker, refusals[0].Detail)
	}
}

// TestTheRCBundleCarriesNoAttestation guards the one thing this session must never do: ship a bundle whose
// manifest claims operator legs that have not run.
func TestTheRCBundleCarriesNoAttestation(t *testing.T) {
	m := committedManifest(t)
	if att, ok := m["operator_attestation"]; ok && att != nil {
		t.Fatalf("the committed RC bundle carries an operator_attestation (%v) — no §6 leg has been executed in this program, so any attestation here is a fabrication", att)
	}
	if maturity, _ := m["maturity"].(string); maturity != "rc" {
		t.Errorf("the committed bundle declares maturity %q, want \"rc\" — the local closure of this gate is a release CANDIDATE", maturity)
	}
	if release, _ := m["release"].(string); strings.Contains(strings.ToLower(release), "stable") {
		t.Errorf("the bundle is named %q — a bundle named stable is a stable claim, and this session makes none", release)
	}
}

// TestTheAttestationLedgerMatchesTheTriageTable keeps the gate's canonical §6 ledger equal to the document
// the operator reads. A gate demanding legs the triage table does not list, or missing one it does, would
// send an operator to satisfy a different set of obligations than the one that gates them.
func TestTheAttestationLedgerMatchesTheTriageTable(t *testing.T) {
	doc := readRepoFile(t, "docs/operations/known-gaps-1.0.md")
	for _, leg := range uat.StableAttestationLegs {
		if !strings.Contains(doc, "`"+leg.ID+"`") {
			t.Errorf("the promote gate demands §6 leg %s but docs/operations/known-gaps-1.0.md does not list it", leg.ID)
		}
	}
	// ...and the reverse: every `L-n` row in the triage table's operator-leg section must be in the ledger.
	for _, row := range strings.Split(doc, "\n") {
		id, ok := legRowID(row)
		if !ok {
			continue
		}
		found := false
		for _, leg := range uat.StableAttestationLegs {
			if leg.ID == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("docs/operations/known-gaps-1.0.md lists operator leg %s but the promote gate does not demand it — an obligation the gate cannot name is one nobody has to meet", id)
		}
	}
}

// legRowID extracts the leg id from a triage-table row of the form "| `L-1` | ... |".
func legRowID(row string) (string, bool) {
	trimmed := strings.TrimSpace(row)
	if !strings.HasPrefix(trimmed, "| `L-") {
		return "", false
	}
	rest := strings.TrimPrefix(trimmed, "| `")
	id, _, ok := strings.Cut(rest, "`")
	return id, ok
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}
