// The E28 T4 exit gate's REFUSAL matrix (plan §T4): for each of the eight counters, a manifest that is
// shape-consistent and claims a property it did not earn.
//
// EVERY NEGATIVE MOVES A LEDGER ROW AND LEAVES THE DECLARED COUNTER WHERE IT WAS, which is the only mutation
// that tests the RE-DERIVATION rather than the arithmetic. Lowering both together tests that a number equals
// itself; moving one tests that the gate reads the bytes. Half of these counters are EQUALITIES rather than
// zeros — approver entries in and out, declared routes and scanned routes — and an equality is the shape a
// hand-written proof gets wrong in the direction nobody notices.
//
// AND THE CONTROL COMES FIRST. TestTheCommittedBundlePassesItsOwnGate is what every negative below is measured
// against: without it, a gate that refused EVERYTHING would report the same green as one that discriminates,
// which is this repository's most-found defect wearing a different hat.
package fleetconsole

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// committedManifest reads the bundle every negative below is a mutation of.
func committedManifest(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the committed bundle: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the committed bundle: %v", err)
	}
	return m
}

// mutateProof hands the fleet-console proof body to `edit` and returns the re-encoded manifest.
func mutateProof(t *testing.T, edit func(proof map[string]any)) []byte {
	t.Helper()
	m := committedManifest(t)
	cases, _ := m["cases"].([]any)
	found := false
	for _, raw := range cases {
		c, _ := raw.(map[string]any)
		proof, ok := c["fleet_console_proof"].(map[string]any)
		if !ok {
			continue
		}
		edit(proof)
		found = true
	}
	if !found {
		t.Fatal("the committed bundle carries no fleet_console_proof to mutate")
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return out
}

// rows decodes one of the proof's ledgers into editable maps.
func rows(t *testing.T, proof map[string]any, field string) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(proof[field])
	if err != nil {
		t.Fatalf("marshal %s: %v", field, err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", field, err)
	}
	return out
}

func refusalsMention(refusals []uat.Refusal, want string) bool {
	for _, r := range refusals {
		if strings.Contains(r.Detail, want) {
			return true
		}
	}
	return false
}

// TestTheCommittedBundlePassesItsOwnGate is the control every negative below is measured against.
func TestTheCommittedBundlePassesItsOwnGate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the committed bundle: %v", err)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed bundle does not pass its own gate at rc — every negative below would then be vacuous:\n%v", refusals)
	}
}

// TestThePromoteGateRefusesStable is the no-tier-advance rule, mechanically. `console` closes PREVIEW.
func TestThePromoteGateRefusesStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the committed bundle: %v", err)
	}
	if refusals := uat.PromoteGateFor(raw, "stable"); len(refusals) == 0 {
		t.Fatal("the fleet-console bundle was promotable to STABLE — no tier advances in this release, and a management screen is not evidence about the plane it manages")
	}
}

// TestADroppedClaimMarkerStillRoutesHere is the promote-gate-family-dispatch rule, asserted rather than
// assumed: the family is recognized by the CASE IDS, never by the claim this gate enforces. Dropping the
// marker must NOT reroute the bundle to a weaker gate that would pass it.
func TestADroppedClaimMarkerStillRoutesHere(t *testing.T) {
	m := committedManifest(t)
	cases, _ := m["cases"].([]any)
	for _, raw := range cases {
		c, _ := raw.(map[string]any)
		delete(c, "fleet_console_claim")
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	refusals := uat.PromoteGateFor(out, "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle with its fleet_console_claim DROPPED passed — it rerouted to a gate that knows none of this epic's guards, which is the promote-gate-family-dispatch defect this tree has shipped once already")
	}
	if !refusalsMention(refusals, "COMPLETE FleetConsoleProof") {
		t.Errorf("the dropped-claim bundle was refused, but not BY THIS GATE — the refusal must come from the E28 family, or the dispatch found a different owner:\n%v", refusals)
	}
}

// TestTheE2xFamiliesAreDisjoint pins the assumption PromoteGateFor's E28 clause rests on. The clause is
// checked FIRST, so if an E28 id also matched an earlier family the order would be silently load-bearing —
// and if a LATER family's id matched E28's, that family's bundles would be judged by this gate.
func TestTheE2xFamiliesAreDisjoint(t *testing.T) {
	owners := map[string][]string{
		"uat.FleetConsoleCaseIDs": uat.FleetConsoleCaseIDs,
		"uat.BackgroundCaseIDs":   uat.BackgroundCaseIDs,
		"uat.AdminConsoleCaseIDs": uat.AdminConsoleCaseIDs,
		"uat.FleetCaseIDs":        uat.FleetCaseIDs,
		"uat.ToolApprovalCaseIDs": uat.ToolApprovalCaseIDs,
		"uat.CodeAndShipCaseIDs":  uat.CodeAndShipCaseIDs,
		"uat.ToolsMemoryCaseIDs":  uat.ToolsMemoryCaseIDs,
		"uat.AgentSurfaceCaseIDs": uat.AgentSurfaceCaseIDs,
	}
	names := make([]string, 0, len(owners))
	for name := range owners {
		names = append(names, name)
	}
	slices.Sort(names)
	for i, left := range names {
		for _, right := range names[i+1:] {
			for _, id := range owners[left] {
				if slices.Contains(owners[right], id) {
					t.Errorf("%s is claimed by BOTH %s and %s — PromoteGateFor dispatches on case-id MEMBERSHIP, so an id in two families is a bundle two gates both think they own, and the one that runs is whichever clause is written first", id, left, right)
				}
			}
		}
	}
}

// --- (a) the birth path ---------------------------------------------------------------------------------

func TestAPoolLedgerWithNoUnsandboxedHostPostureIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "pool_ledger")
		for _, row := range list {
			row["posture"] = "sandboxed-linux"
		}
		proof["pool_ledger"] = list
		// The declared posture set is left EXACTLY as it was, which is the point: a gate that recomputed
		// nothing would compare a number to itself.
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "`unsandboxed-host`") {
		t.Errorf("a bundle whose pools are all sandboxed-linux was not refused for it — that is the value migration 000045 declared and no code path could write, so a release without it certifies the hole rather than the fix:\n%v", refusals)
	}
}

func TestASeededPoolIsNotABirthPath(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "pool_ledger")
		list[0]["created_via"] = "seed"
		proof["pool_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); len(refusals) == 0 {
		t.Error("a pool created by the SEED was accepted as evidence of a birth path — InsertDefaultRunnerPool wrote its posture as a LITERAL for this repository's whole life, so a seeded pool is evidence of the hole")
	}
}

func TestADeclaredPoolCountThatDisagreesWithTheLedgerIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "pool_ledger")
		proof["pool_ledger"] = list[:len(list)-1] // one fewer row, count untouched
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "never from the manifest") {
		t.Errorf("a shortened pool ledger with the declared count left in place was not refused:\n%v", refusals)
	}
}

// --- (b) the waiting room -------------------------------------------------------------------------------

func TestAMachineThatNeverWaitedIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "waiting_room_ledger")
		list[0]["reached_pending"] = false
		proof["waiting_room_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "never actually reached `pending`") {
		t.Errorf("a machine that walked straight through was accepted as evidence of a waiting room:\n%v", refusals)
	}
}

// TestAnAdmissionFromTheCLIAloneIsRefused is the sharpest row in this group: E24 T6 ALREADY proved the approve
// route from a component test, so a bundle whose only admission came from a CLI or an API call certifies E24
// again rather than E28.
func TestAnAdmissionFromTheCLIAloneIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "waiting_room_ledger")
		for _, row := range list {
			row["admitted_from"] = "cli"
		}
		proof["waiting_room_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "NONE was admitted from the console") {
		t.Errorf("a release whose every admission came from outside the console was not refused — an epic whose crown claim is a screen cannot certify it from a CLI transcript:\n%v", refusals)
	}
}

// --- (c) the minted value -------------------------------------------------------------------------------

func TestAKeyValueFoundInASiteIsRefusedWhileTheZeroStandsStill(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "key_scan_ledger")
		list[0]["key_value_hits"] = 1
		proof["key_scan_ledger"] = list
		// `key_values_found` stays 0.
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "minted key VALUE was found") {
		t.Errorf("a key value in a landing site was not refused while the declared zero stood still:\n%v", refusals)
	}
}

func TestAKeyScanSiteScannedWithoutDecodingIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "key_scan_ledger")
		list[0]["decoded_before_scanning"] = false
		proof["key_scan_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "WITHOUT decoding") {
		t.Errorf("a site scanned over raw encoded bytes was accepted — that sweep can never fail, which this tree measured in E14 T7 and paid for again in E20 T4:\n%v", refusals)
	}
}

func TestAKeyScanSiteWithNoProbeItFoundIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "key_scan_ledger")
		list[0]["probe_found"] = false
		proof["key_scan_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "names no probe it actually found") {
		t.Errorf("a site whose haystack was never shown to be readable was accepted:\n%v", refusals)
	}
}

// TestTheLaterResponseSiteCannotBeDropped is the site the other four pass over, and it is the one a tidy
// build removes: a value can be gone from the document and still be served back by a list call.
func TestTheLaterResponseSiteCannotBeDropped(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "key_scan_ledger")
		kept := list[:0]
		for _, row := range list {
			if row["site"] == "later-response" {
				continue
			}
			kept = append(kept, row)
		}
		proof["key_scan_ledger"] = kept
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "later-response") {
		t.Errorf("dropping the later-response site was not refused:\n%v", refusals)
	}
}

// --- (d) the policy document ----------------------------------------------------------------------------

// TestAShrunkenApproverListIsRefused is the security row: an empty approver list PERMITS EVERY PRINCIPAL.
func TestAShrunkenApproverListIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "policy_ledger")
		list[0]["approvers_after"] = 0
		proof["policy_ledger"] = list
		// `approver_entries_after` stays where it was.
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "PERMITS EVERY PRINCIPAL") {
		t.Errorf("a policy write that erased an approver list was not refused — HIL-P11 measured that an empty list admits every principal, so this is the approval gate opening silently:\n%v", refusals)
	}
}

// TestAPartialPolicyRequestIsRefusedEvenWhenTheOutcomeSurvived is the half a stored-outcome assertion cannot
// see: a server that MERGED would let "the approvers survived" pass over a form still sending one field.
func TestAPartialPolicyRequestIsRefusedEvenWhenTheOutcomeSurvived(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "policy_ledger")
		list[0]["fields_in_request"] = 1 // the outcome is untouched: before == after, still
		proof["policy_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "fewer than all") {
		t.Errorf("a one-field policy request whose OUTCOME survived was accepted — that is exactly the build a merging server would make green, and the next server change would break the screen silently:\n%v", refusals)
	}
}

// TestAPolicyWriteStartingFromAnEmptyListIsRefused closes the vacuous-pass direction: "the approvers survived"
// is trivially true of a project that had none.
func TestAPolicyWriteStartingFromAnEmptyListIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "policy_ledger")
		for _, row := range list {
			row["approvers_before"] = 0
			row["approvers_after"] = 0
		}
		proof["policy_ledger"] = list
		proof["approver_entries_before"] = 0
		proof["approver_entries_after"] = 0
	})
	if refusals := uat.PromoteGateFor(out, "rc"); len(refusals) == 0 {
		t.Error("a policy write measured against an ALREADY EMPTY approver list was accepted — the equality holds and proves nothing, which is the classic vacuous pass")
	}
}

// --- (e) the routes -------------------------------------------------------------------------------------

func TestAnUnscannedRouteIsRefusedWhileTheCountStandsStill(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "route_ledger")
		list[0]["axe_scanned_in"] = []string{"light"}
		proof["route_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "not axe-scanned in every colour scheme") {
		t.Errorf("a route scanned in ONE colour scheme was accepted — the light-only scan is precisely the hole E25 closed:\n%v", refusals)
	}
}

func TestARouteLedgerMissingAPageThisEpicOpenedIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "route_ledger")
		kept := list[:0]
		for _, row := range list {
			if row["path"] == "/fleet" {
				continue
			}
			kept = append(kept, row)
		}
		proof["route_ledger"] = kept
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "/fleet") {
		t.Errorf("a route ledger with this epic's own fleet page removed was accepted — the count would still be internally consistent, over a console one page smaller:\n%v", refusals)
	}
}

// --- (f) the confirmation split -------------------------------------------------------------------------

func TestAnIrreversibleActionOnTheNativeConfirmIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "action_ledger")
		for _, row := range list {
			if row["reversible"] == false {
				row["confirmation"] = uat.FleetConsoleNativeConfirm
				break
			}
		}
		proof["action_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "wrong side of the confirmation split") {
		t.Errorf("an irreversible action left on window.confirm was accepted — SC 3.3.4 leg 1 is unreachable for a revoke and leg 3's REVIEWING is a data call:\n%v", refusals)
	}
}

// TestAReversibleActionBehindAnAlertDialogIsAlsoRefused is the direction a one-way check misses, and it is the
// change the next reader makes "for consistency".
func TestAReversibleActionBehindAnAlertDialogIsAlsoRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "action_ledger")
		for _, row := range list {
			if row["reversible"] == true {
				row["confirmation"] = uat.FleetConsoleAlertDialog
				break
			}
		}
		proof["action_ledger"] = list
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "wrong side of the confirmation split") {
		t.Errorf("a reversible action promoted into an alertdialog was accepted — a console that guards everything has stopped distinguishing rather than become more careful:\n%v", refusals)
	}
}

// --- (g) the ceilings -----------------------------------------------------------------------------------

func TestACeilingNoScreenStatesIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		list := rows(t, proof, "ceiling_ledger")
		kept := list[:0]
		for _, row := range list {
			if row["gap_id"] == "FLT-P15" {
				continue
			}
			kept = append(kept, row)
		}
		proof["ceiling_ledger"] = kept
	})
	if refusals := uat.PromoteGateFor(out, "rc"); !refusalsMention(refusals, "FLT-P15") {
		t.Errorf("a bundle whose screens stopped stating FLT-P15 was accepted — that is the row known-gaps says to read before any other FLT-P*, because it bounds what all of them are worth:\n%v", refusals)
	}
}

// --- (h) the conformance sweep --------------------------------------------------------------------------

func TestASweepThatDidNotRiseAboveThePreE28FloorIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		var subjects []string
		raw, err := json.Marshal(proof["conformance_ledger"])
		if err != nil {
			t.Fatalf("marshal conformance ledger: %v", err)
		}
		if err := json.Unmarshal(raw, &subjects); err != nil {
			t.Fatalf("decode conformance ledger: %v", err)
		}
		proof["conformance_ledger"] = subjects[:uat.FleetConsoleSweepFloorBeforeE28]
	})
	if refusals := uat.PromoteGateFor(out, "rc"); len(refusals) == 0 {
		t.Error("a conformance sweep comparing no more collections than before this epic was accepted — E28 put a fleet surface on both sides of it, so a floor that did not move means the new surface is not in it")
	}
}

// --- the contract ledger ---------------------------------------------------------------------------------

func TestAnEditedContractLedgerIsRefused(t *testing.T) {
	out := mutateProof(t, func(proof map[string]any) {
		var reqs []map[string]any
		raw, err := json.Marshal(proof["contracts"])
		if err != nil {
			t.Fatalf("marshal contracts: %v", err)
		}
		if err := json.Unmarshal(raw, &reqs); err != nil {
			t.Fatalf("decode contracts: %v", err)
		}
		proof["contracts"] = reqs[:len(reqs)-1] // digest untouched
	})
	if refusals := uat.PromoteGateFor(out, "rc"); len(refusals) == 0 {
		t.Error("a bundle with a §3.5 row deleted and its digest untouched was accepted — the digest is derived from the CODE table, so it cannot be made self-consistent from inside a manifest")
	}
}

// TestTheFleetConsoleLedgerRefusesTheUnconfirmedRow pins §3.5's own rule, for the sixth time this tree has had
// to state it: a vendor SILENCE is not a design freedom. W5 could not be confirmed and is carried as an
// absence — it entered no code, and no test, document or bundle says the dialog's default focus is a
// requirement.
func TestTheFleetConsoleLedgerRefusesTheUnconfirmedRow(t *testing.T) {
	found := false
	for _, req := range uat.FleetConsoleContracts {
		if req.Divergence != "W5" {
			continue
		}
		found = true
		if !strings.Contains(req.SourceURL, "UNCONFIRMED") {
			t.Errorf("W5's source no longer says UNCONFIRMED (%q) — a row that gained a citation must gain the citation, not the label", req.SourceURL)
		}
		if !strings.Contains(req.Requirement, "FLC-P5") {
			t.Error("W5 does not name the known-gaps row it was filed as — an unconfirmed fact with no gap row is a fact nobody will re-check")
		}
	}
	if !found {
		t.Error("the contract ledger no longer carries W5 — the UNCONFIRMED row is the one that records what this epic could NOT establish, and dropping it makes the ledger read as though everything was confirmed")
	}
}

// TestNoTierAdvancesInThisRelease is the E28 half of the standing rule, asserted against the SHIPPED
// capability table rather than against a sentence in a comment.
func TestNoTierAdvancesInThisRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m struct {
		Cases []struct {
			ID   string `json:"id"`
			Tier *struct {
				Capabilities []struct {
					Capability   string `json:"capability"`
					DeclaredTier string `json:"declared_tier"`
				} `json:"capabilities"`
				ClaimsDigest string `json:"claims_digest"`
			} `json:"capability_tier_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	// The bundle inherits the E17 tier proof unchanged, and the assertion that matters is twofold: `console`
	// is still PREVIEW, and the CLAIMS DIGEST is the one the shipped code derives. The second is the stronger
	// half — E28 added no capability at all, and a new member on CapabilityTierOrder would move
	// CapabilityClaimsDigest and redden every case checksum in every committed bundle, which is exactly why
	// this epic opened none.
	seen := 0
	for _, c := range m.Cases {
		if c.Tier == nil {
			continue
		}
		seen++
		if c.Tier.ClaimsDigest != uat.CapabilityClaimsDigest() {
			t.Errorf("%s carries a claims digest the shipped table does not derive — E28 added no capability, so this digest must be byte-identical to the one every committed bundle already folds into its checksums", c.ID)
		}
		for _, cap := range c.Tier.Capabilities {
			if cap.Capability == "console" && cap.DeclaredTier != "preview" {
				t.Errorf("%s carries console at tier %q — E28 closes PREVIEW, and with FLT-P15 open a fleet screen implies more than it proves, so raising it would formalise the implication the screens' own sentences exist to prevent", c.ID, cap.DeclaredTier)
			}
		}
	}
	if seen == 0 {
		t.Fatal("the bundle carries no capability_tier_proof at all — the inherited E17 anchor is what makes \"no tier advanced\" a statement about something")
	}
}
