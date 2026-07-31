package background

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE REFUSAL MATRIX (plan §T7). Every counter this bundle declares is RE-DERIVED from the bytes it carries,
// and this file is what proves each re-derivation is LOAD-BEARING rather than decorative.
//
// THE SHAPE OF EVERY CASE BELOW IS THE SAME AND IT IS THE ONLY SHAPE THAT PROVES ANYTHING. A manifest is
// mutated so that ONE LEDGER ROW no longer supports the counter beside it, while the DECLARED counter is left
// exactly where it was. A gate that believed the manifest would report the same green as before; a gate that
// re-derives says which claim failed. Mutating the counter instead would be a weaker test — it would only
// prove the two fields are compared, not that the bytes decide.
//
// The mutations are applied to the REAL committed bundle rather than to a hand-built fixture, so every one of
// them also proves the committed value is the value the sweep produces: flip it and the release fails.

// mutate reads the committed bundle, hands the decoded background proof to fn, and returns the re-encoded
// manifest. It fails the test if the bundle does not verify clean BEFORE the mutation, because a mutation
// applied to an already-red bundle proves nothing about the guard.
func mutate(t *testing.T, fn func(proof map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if findings := uat.VerifyManifest(raw, nil); len(findings) != 0 {
		t.Fatalf("the committed bundle must verify clean before it is mutated: %v", findings)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	found := false
	for _, c := range m["cases"].([]any) {
		cs := c.(map[string]any)
		proof, ok := cs["background_proof"].(map[string]any)
		if !ok {
			continue
		}
		found = true
		fn(proof)
	}
	if !found {
		t.Fatal("the committed bundle carries no background_proof to mutate")
	}
	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return out
}

// refuses asserts the mutated manifest is refused by BOTH the shape verifier and the promote gate, and that
// the promote refusal names the claim rather than only saying "incomplete".
func refuses(t *testing.T, mutated []byte, wantDetail string) {
	t.Helper()
	if findings := uat.VerifyManifest(mutated, nil); len(findings) == 0 {
		t.Error("the shape verifier accepted a proof whose ledger no longer supports its counter — Complete() is not re-deriving")
	}
	refusals := uat.BackgroundPromoteGate(mutated, "rc")
	if len(refusals) == 0 {
		t.Fatal("the promote gate accepted a proof whose ledger no longer supports its counter")
	}
	joined := ""
	for _, r := range refusals {
		joined += r.Detail + "\n"
	}
	if !strings.Contains(joined, wantDetail) {
		t.Errorf("the refusal does not name the failed claim (want a mention of %q):\n%s", wantDetail, joined)
	}
}

// --- (a) the six semantics -------------------------------------------------------------------------------

// TestDroppingTheDefiningSemanticIsRefused is the sharpest case in this file. §2.6 — the model called another
// tool while the process was still running — is the one claim a weaker build silently drops, because "it ran
// in the background" is TRUE of a park-and-wait. The proof's declared count is lowered honestly to five here,
// so the refusal cannot come from an arithmetic mismatch: what must fail is the ABSENCE of that semantic.
func TestDroppingTheDefiningSemanticIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["semantics_ledger"])
		kept := rows[:0]
		for _, r := range rows {
			if int(r["semantic"].(float64)) != 6 {
				kept = append(kept, r)
			}
		}
		proof["semantics_ledger"] = kept
		proof["semantics_replicated"] = len(kept)
		proof["semantics_measured_from_the_machine"] = countMachine(kept)
	})
	refuses(t, mutated, "does NOT carry §2.6")
}

// TestASemanticMeasuredFromOurOwnBookkeepingIsRefused pins the vocabulary. `bookkeeping` is not one of the
// four sources on purpose: a liveness claim read off a row we wrote is a claim about the row, and this whole
// epic's proof discipline is that the kernel and the daemon answer questions about processes.
func TestASemanticMeasuredFromOurOwnBookkeepingIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["semantics_ledger"])
		rows[5]["measured_from"] = "bookkeeping"
		proof["semantics_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// TestASemanticsCountAboveItsLedgerIsRefused is the plain re-derivation: the declared number stays, one row
// goes.
func TestASemanticsCountAboveItsLedgerIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["semantics_ledger"])
		proof["semantics_ledger"] = rows[:len(rows)-1] // §2.6 is last; drop §2.5 instead
	})
	// Dropping the LAST row removes §2.6, so this reports the defining-semantic refusal — which is itself the
	// stronger statement. The count arm is exercised by the next test, which drops a middle row.
	refuses(t, mutated, "does NOT carry §2.6")
}

func TestDroppingANonDefiningSemanticIsAlsoRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["semantics_ledger"])
		proof["semantics_ledger"] = append(rows[:2], rows[3:]...) // drop §2.3, keep §2.6
	})
	refuses(t, mutated, "of §2's 6 semantics")
}

// --- (b) the refusals ------------------------------------------------------------------------------------

// TestARefusalThatStartedAProcessIsRefused is the security half. A gated tool that spawns before a human
// decides has performed the side effect the gate exists to hold back.
func TestARefusalThatStartedAProcessIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["refusal_ledger"])
		rows[0]["spawns"] = float64(1)
		proof["refusal_ledger"] = rows
		// The declared counter stays at zero — which is the whole point of the mutation.
	})
	refuses(t, mutated, "were started under a refusal")
}

// TestARefusalWithNoNonVacuityControlIsRefused is the clause this epic's own fork point taught: T2's
// approval-gate RED PASSED before anything was written, because there was no spawn path at all.
func TestARefusalWithNoNonVacuityControlIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["refusal_ledger"])
		rows[0]["control_count"] = float64(0)
		proof["refusal_ledger"] = rows
	})
	refuses(t, mutated, "carry NO non-vacuity control")
}

// TestARefusalControlWithNoStatedUnitIsRefused pins the field this gate added after measuring that one
// refusal's honest control is a SYNCHRONOUS run rather than a background spawn: a flattened integer would
// have been a number nobody could attribute.
func TestARefusalControlWithNoStatedUnitIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["refusal_ledger"])
		rows[1]["control_unit"] = "things"
		proof["refusal_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// --- (c) ownership ---------------------------------------------------------------------------------------

// TestSignallingAHandleWeCannotProveIsOursIsRefused is the sharpest absence in the bundle.
func TestSignallingAHandleWeCannotProveIsOursIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["ownership_ledger"])
		rows[0]["signals_to_unprovable_handles"] = float64(1)
		proof["ownership_ledger"] = rows
	})
	refuses(t, mutated, "could not prove was ours")
}

// TestOnePostureStandingInForBothIsRefused: a container's lifetime is the daemon's business and a process
// group's is the kernel's, and the plan measured that before this epic NEITHER survived, for two entirely
// different reasons.
func TestOnePostureStandingInForBothIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["ownership_ledger"])
		proof["ownership_ledger"] = rows[:1]
		proof["postures_that_outlive_their_call"] = float64(1)
	})
	refuses(t, mutated, "of 2 postures")
}

// TestAPostureWhoseProcessDidNotSurviveIsRefused is the one thing this epic exists to make true.
func TestAPostureWhoseProcessDidNotSurviveIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["ownership_ledger"])
		rows[1]["alive_after_the_call_returned"] = false
		proof["ownership_ledger"] = rows
	})
	refuses(t, mutated, "still alive after the call returned")
}

// --- (d) the notice --------------------------------------------------------------------------------------

// TestTwoNoticesForOneSettledTaskIsRefused is the duplicate the single-winner UPDATE exists to prevent, and
// the epic measured its own mutation of it: removing `notified_at IS NULL` produces FOUR.
func TestTwoNoticesForOneSettledTaskIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["notice_ledger"])
		rows[1]["notices"] = float64(2)
		proof["notice_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// TestInterruptingARunThatNeverParkedIsRefused: a wake fired at a run that is still working is a SECOND
// attempt of one run.
func TestInterruptingARunThatNeverParkedIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["notice_ledger"])
		rows[2]["interrupted_a_running_run"] = true
		proof["notice_ledger"] = rows
	})
	refuses(t, mutated, "interrupted a run that never parked")
}

// TestANoticeScenarioWithNoMutationIsRefused: a property nobody has broken is a property nobody has tested.
func TestANoticeScenarioWithNoMutationIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["notice_ledger"])
		rows[3]["mutation"] = ""
		proof["notice_ledger"] = rows
	})
	refuses(t, mutated, "name no mutation")
}

// --- (e) the reaper --------------------------------------------------------------------------------------

// TestAMissingReaperDutyIsRefused: each duty is a DIFFERENT way an orphan happens.
func TestAMissingReaperDutyIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["reaper_ledger"])
		proof["reaper_ledger"] = rows[:len(rows)-1]
		proof["reaper_duties_measured_from_the_machine"] = float64(len(rows) - 1)
	})
	refuses(t, mutated, "reaper duty(ies) are absent")
}

// TestAReaperOutcomeReadOffOurOwnRecordIsRefused: a reaper is the component most able to satisfy its own
// tests, and E24 measured `runners.capacity` written, read back and used in no decision.
func TestAReaperOutcomeReadOffOurOwnRecordIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["reaper_ledger"])
		rows[0]["measured_from"] = "bookkeeping"
		proof["reaper_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// --- (f) the credential ----------------------------------------------------------------------------------

// TestAnEnvironmentValueInALandingSiteIsRefused: both sites T2 and T4 each recorded as OPEN, closed by T6
// from one function. A hit here means one of them is open again.
func TestAnEnvironmentValueInALandingSiteIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["redaction_ledger"])
		rows[1]["sentinel_hits"] = float64(1)
		proof["redaction_ledger"] = rows
	})
	refuses(t, mutated, "was found 1 time(s) in a landing site")
}

// TestASiteScannedWithoutDecodingIsRefused is this repository's own lesson, twice paid for: a raw-byte sweep
// over encoded output can never fail, so a green from one certifies nothing.
func TestASiteScannedWithoutDecodingIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["redaction_ledger"])
		rows[0]["decoded"] = false
		proof["redaction_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// TestASiteWithNoProbeIsRefused: a haystack nobody has shown was read is not a haystack.
func TestASiteWithNoProbeIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["redaction_ledger"])
		rows[2]["probe_found"] = false
		proof["redaction_ledger"] = rows
	})
	refuses(t, mutated, "cannot be swept")
}

// TestADroppedLandingSiteIsRefused: the two sites T2 and T4 recorded as open are exactly the two a later
// reader would assume were covered.
func TestADroppedLandingSiteIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := decodeRows(t, proof["redaction_ledger"])
		proof["redaction_ledger"] = rows[1:]
	})
	refuses(t, mutated, "cannot be swept")
}

// --- the machine and the contract ledger -----------------------------------------------------------------

// TestAProofClaimingAMachineOtherThanLocalIsRefused is the structural ceiling. There is no peer here at all:
// E24 T7's execution relay was never shipped, so every measurement in this release was taken on the control
// plane's own box. A field that could say "real" would be a field that could lie about a topology this epic
// does not have.
func TestAProofClaimingAMachineOtherThanLocalIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) { proof["machine"] = "real" })
	refuses(t, mutated, "no COMPLETE BackgroundProof")
}

// TestAShrunkenContractLedgerIsRefused: the digest is derived from the CODE table, so a bundle cannot present
// a self-consistent digest over an edited ledger.
func TestAShrunkenContractLedgerIsRefused(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {
		rows := proof["contracts"].([]any)
		proof["contracts"] = rows[:len(rows)-1]
	})
	refuses(t, mutated, "no COMPLETE BackgroundProof")
}

// TestTheBackgroundLedgerRefusesTheUnconfirmedRows keeps §3.5's UNCONFIRMED rows OUT of the contract ledger.
// P9 (whether Docker keeps a stopped container's logs) and P11 (the harness's own default command timeout)
// could not be read on any published page, and a ledger of "published requirements we implement" is the wrong
// home for something nobody could read — putting them there would make an unread page look like a citation.
// Neither entered a test, a doc sentence or a bundle field.
func TestTheBackgroundLedgerRefusesTheUnconfirmedRows(t *testing.T) {
	for _, row := range uat.BackgroundContracts {
		if row.Divergence == "P9" || row.Divergence == "P11" {
			t.Errorf("%s is UNCONFIRMED in §3.5 and is in the canonical contract ledger — it could not be read on any published page, so citing it would make an unread page look like a source", row.Divergence)
		}
		if strings.Contains(strings.ToUpper(row.Requirement), "UNCONFIRMED") {
			t.Errorf("%s carries the word UNCONFIRMED in a ledger of requirements the code IMPLEMENTS", row.Divergence)
		}
		if strings.TrimSpace(row.SourceURL) == "" {
			t.Errorf("%s cites no source", row.Divergence)
		}
	}
	// And the positive half, so a ledger emptied of everything would not pass the loop above by having no rows.
	if len(uat.BackgroundContracts) < 5 {
		t.Fatalf("the contract ledger holds %d row(s) — a loop over an empty ledger reports the same green as one over a correct one", len(uat.BackgroundContracts))
	}
}

// --- the promote gate's routing --------------------------------------------------------------------------

// TestThePromoteGateRoutesToE26AndPassesTheRC is the positive half: the committed bundle passes at `rc`.
func TestThePromoteGateRoutesToE26AndPassesTheRC(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed background bundle was refused at rc: %v", refusals)
	}
}

// TestThePromoteGateRefusesStable is the tier decision, mechanically. `stable` is refused by the composed
// chain because no tier moved and the §6 legs underneath are open — and E26 adds its own reason: making a
// tool asynchronous is not evidence about the plane that tool runs on.
func TestThePromoteGateRefusesStable(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if refusals := uat.PromoteGateFor(raw, "stable"); len(refusals) == 0 {
		t.Fatal("the background bundle was promoted to STABLE — no tier advanced in this epic and the §6 legs beneath it are open")
	}
}

// TestADroppedBackgroundClaimStillRoutesHere is the promote-gate-family-dispatch guard, and it is the reason
// the family is recognized by the CASE IDS rather than by the claim the gate enforces. A release that dropped
// its `background_claim` must still be judged by THIS gate — if it rerouted to AdminConsolePromoteGate it
// would pass, because that gate knows nothing about any of the six groups above.
//
// THIS IS THE FOURTH REGISTRATION POINT, ASSERTED. A bundle can be in committedBundleSurfaces, in the
// caseChecksumParts branch and in promoteFamilies and still route to the WRONG gate, because none of those
// three decides dispatch — the `for _, c := range m.Cases` clause in PromoteGateFor does.
func TestADroppedBackgroundClaimStillRoutesHere(t *testing.T) {
	mutated := mutate(t, func(proof map[string]any) {})
	var m map[string]any
	if err := json.Unmarshal(mutated, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range m["cases"].([]any) {
		cs := c.(map[string]any)
		delete(cs, "background_claim")
		delete(cs, "background_proof")
	}
	stripped, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	refusals := uat.PromoteGateFor(stripped, "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle that DROPPED its background claim was promoted — it rerouted to a weaker family gate, which is the promote-gate-family-dispatch defect this tree has shipped once already")
	}
	joined := ""
	for _, r := range refusals {
		joined += r.Detail + "\n"
	}
	if !strings.Contains(joined, "no COMPLETE BackgroundProof") {
		t.Errorf("the refusal did not come from the E26 gate, so dispatch fell through to a weaker family:\n%s", joined)
	}
}

// --- helpers ----------------------------------------------------------------------------------------------

// decodeRows turns a carried ledger (json.RawMessage on the way out, []any on the way back in) into a list of
// mutable maps.
func decodeRows(t *testing.T, v any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-marshal the ledger: %v", err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode the ledger rows: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("the ledger decoded to zero rows — the mutation would then be applied to nothing")
	}
	return rows
}

// countMachine re-derives how many rows were measured from the kernel or the daemon, so a mutation that drops
// a row can lower the declared counter HONESTLY and leave only the property under test failing.
func countMachine(rows []map[string]any) int {
	n := 0
	for _, r := range rows {
		switch r["measured_from"] {
		case "kernel", "daemon":
			n++
		}
	}
	return n
}
