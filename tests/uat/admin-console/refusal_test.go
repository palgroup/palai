package adminconsole

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE REFUSAL MATRIX (plan §T9) — and its shape is the whole reason this file exists rather than a comment
// saying "the counters are re-derived".
//
// Every negative below is a SHAPE-CONSISTENT manifest: the bundle verifies clean, every field is present and
// well-typed, and the declared counter is left EXACTLY where the honest bundle put it. What changes is one row
// of one carried ledger — so the only thing that can catch it is a gate that RECOMPUTES the counter from the
// rows instead of reading the number. A declared-number gate passes every one of these.
//
// Two families, and both are needed:
//
//	MOVE THE LEDGER, LEAVE THE NUMBER. An ungated relay method, an unscanned route, a sentinel hit, a third
//	ciphertext statement, a runbook step below /v1, an approval that applied on a mismatched hash. These
//	prove the recompute is load-bearing.
//
//	DELETE THE DEMONSTRATION A ZERO RESTS ON. A byte-scan layer with no probe it found, a repaired ledger
//	row with no re-observation, a queue that never refused a stale binding. These prove the NON-VACUITY
//	checks are load-bearing — which is the half this repository has shipped defeated more often, because a
//	zero over an empty corpus looks identical to a zero over a corpus that had every chance to be non-zero.

// mutate reads the committed bundle, applies fn to the decoded admin-console proof of the FIRST case carrying
// one, and returns the re-encoded manifest. Everything else about the bundle is untouched, so a refusal can
// only come from the mutation.
func mutate(t *testing.T, fn func(proof map[string]any)) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	touched := false
	for _, c := range m["cases"].([]any) {
		cs := c.(map[string]any)
		proof, ok := cs["admin_console_proof"].(map[string]any)
		if !ok {
			continue
		}
		fn(proof)
		touched = true
		break
	}
	if !touched {
		t.Fatal("the committed bundle carries no admin_console_proof to mutate — this whole file would be vacuous")
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return out
}

// ledgerRows decodes one of the proof's carried ledgers into a mutable slice of rows.
func ledgerRows(t *testing.T, proof map[string]any, field string) []any {
	t.Helper()
	rows, ok := proof[field].([]any)
	if !ok {
		t.Fatalf("the proof's %s is not a JSON array: %T", field, proof[field])
	}
	if len(rows) == 0 {
		t.Fatalf("the proof's %s is empty, so mutating it proves nothing", field)
	}
	return rows
}

// TestTheCommittedBundlePassesItsOwnGate is the control every negative below is measured against. Without it,
// a refusal could be the bundle's rather than the mutation's.
func TestTheCommittedBundlePassesItsOwnGate(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if refusals := uat.AdminConsolePromoteGate(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the committed bundle does not pass its own gate at rc: %v", refusals)
	}
	// And `stable` is REFUSED, by the composed gate underneath: no tier advances in this epic, and the bundle
	// is `rc` maturity.
	if refusals := uat.AdminConsolePromoteGate(raw, "stable"); len(refusals) == 0 {
		t.Fatal("the admin-console bundle was promotable to STABLE — this epic advances no tier and the bundle's own maturity is rc")
	}
}

// TestADroppedClaimMarkerStillRoutesHere is the promote-gate-family-dispatch rule, asserted rather than
// commented (`promote-gate-family-dispatch`, and this tree has shipped the defect once). PromoteGateFor
// recognises the family by CASE-ID MEMBERSHIP, so a manifest that DELETES its admin_console_claim — the very
// thing the gate enforces — must still be judged HERE and refused for the missing claim, never rerouted to a
// weaker family gate that knows none of E25's guards and would pass it.
func TestADroppedClaimMarkerStillRoutesHere(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, c := range m["cases"].([]any) {
		cs := c.(map[string]any)
		delete(cs, "admin_console_claim")
		delete(cs, "admin_console_proof")
	}
	stripped, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	refusals := uat.PromoteGateFor(stripped, "rc")
	if len(refusals) == 0 {
		t.Fatal("a manifest that DROPPED its admin_console_claim was promotable — the claim the gate enforces cannot also be how the gate is selected")
	}
	if !refusalMentions(refusals, "AdminConsoleProof") {
		t.Errorf("the stripped manifest was refused, but not by the E25 gate — it rerouted to a weaker family:\n%v", refusals)
	}
}

// TestTheE24AndE25FamiliesAreDisjoint pins the assumption PromoteGateFor's E25 clause rests on. The two epics
// ran in PARALLEL and neither inherits the other, so the relative order of their two dispatch clauses is
// immaterial TODAY — and "immaterial today" is exactly how a dispatch order becomes load-bearing tomorrow
// without anybody noticing.
func TestTheE24AndE25FamiliesAreDisjoint(t *testing.T) {
	for _, con := range uat.AdminConsoleCaseIDs {
		for _, flt := range uat.FleetCaseIDs {
			if con == flt {
				t.Errorf("%s is claimed by BOTH E24 and E25 — the two dispatch clauses in PromoteGateFor would then be order-dependent, and the earlier one would silently own the other family's bundle", con)
			}
		}
	}
}

// --- family 1: move the ledger, leave the number ----------------------------------------------------------

func TestAnUngatedRelayMethodIsRefusedWhileTheCountStandsStill(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "relay_ledger")
		// The catch-all's DELETE loses its gate. The declared counts are LEFT ALONE, which is the point.
		for _, r := range rows {
			row := r.(map[string]any)
			if row["method"] == "DELETE" {
				row["gate"] = "// TODO: add the session check here"
			}
		}
	})
	assertRefused(t, raw, "open with the session gate")
}

func TestASecondUngatedDoorIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "relay_ledger")
		// A new export claiming the door's excuse. It is not the login route, so the sweep must name it.
		proof["relay_ledger"] = append(rows, map[string]any{
			"file":   "apps/web-console/app/api/palai/admin/route.ts",
			"method": "POST",
			"relay":  false,
			"gate":   "is-the-door: verifyPassword + Origin, issues the session this gate checks",
		})
	})
	assertRefused(t, raw, "is not the login door")
}

func TestAnUnscannedRouteIsRefusedWhileTheCountStandsStill(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "route_ledger")
		// A page scanned in ONE colour scheme only — the exact hole this epic closed, and the one a
		// declared-count gate cannot see.
		rows[len(rows)-1].(map[string]any)["axe_scanned_in"] = []any{"light"}
	})
	assertRefused(t, raw, "not axe-scanned in every colour scheme")
}

func TestARouteWithABlankLeadIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "route_ledger")
		rows[0].(map[string]any)["lead"] = "   "
	})
	assertRefused(t, raw, "no lead")
}

func TestASentinelHitInAResponseBodyIsRefusedWhileTheZeroStandsStill(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "byte_scan_ledger")
		for _, r := range rows {
			row := r.(map[string]any)
			if row["kind"] == "response-body" {
				row["sentinel_hits"] = 1
			}
		}
	})
	assertRefused(t, raw, "the sentinel was found")
}

func TestAThirdCiphertextStatementIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "query_ledger")
		// The plausible one: a query that reads the value back for a console screen. This is the shape the
		// whole layer exists to make expensive, and it lands here before it has a caller, a view or a route.
		for _, r := range rows {
			row := r.(map[string]any)
			if strings.HasSuffix(row["file"].(string), "secrets.sql") {
				row["ciphertext_statements"] = []any{"InsertSecretRef", "ResolveSecretRef", "RevealEnvironmentValue"}
			}
		}
	})
	assertRefused(t, raw, "RevealEnvironmentValue")
}

func TestAShrunkQueryCorpusIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "query_ledger")
		// The parser stopped parsing: every file reports one statement — except secrets.sql, which keeps
		// enough to hold its two ciphertext names. The ciphertext PAIR is therefore untouched and still
		// exactly right, so nothing but the corpus floor can catch this.
		for _, r := range rows {
			row := r.(map[string]any)
			if strings.HasSuffix(row["file"].(string), "secrets.sql") {
				row["statements"] = 2
				continue
			}
			row["statements"] = 1
		}
	})
	assertRefused(t, raw, "under the floor")
}

func TestASweepThatDidNotRiseIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		proof["sweep_items_compared"] = uat.AdminConsoleSweepFloorBeforeE25
		proof["sweep_subjects"] = []any{"GET /v1/organizations", "GET /v1/projects", "GET /v1/api-keys"}
	})
	assertRefused(t, raw, "against a pre-E25 floor")
}

func TestASweepWhoseSubjectsDoNotMatchItsCountIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		subjects := proof["sweep_subjects"].([]any)
		proof["sweep_subjects"] = subjects[:len(subjects)-1]
	})
	assertRefused(t, raw, "subject(s)")
}

func TestARunbookStepBelowTheAPIIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "runbook_ledger")
		rows[3].(map[string]any)["public_api"] = false
	})
	assertRefused(t, raw, "needed something below /v1")
}

func TestAnApprovalThatAppliedOnAMismatchedHashIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "approval_ledger")
		for _, r := range rows {
			row := r.(map[string]any)
			if row["request_hash_matched"] == false {
				row["applied"] = true
				break
			}
		}
	})
	assertRefused(t, raw, "APPLIED while the request hash did not match")
}

// --- family 2: delete the demonstration a zero rests on ---------------------------------------------------

func TestAByteScanLayerWithNoProbeItFoundIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "byte_scan_ledger")
		for _, r := range rows {
			row := r.(map[string]any)
			if row["kind"] == "source-map" {
				row["probe_found"] = false
			}
		}
	})
	assertRefused(t, raw, "names no probe")
}

func TestAMissingByteScanLayerIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "byte_scan_ledger")
		kept := []any{}
		for _, r := range rows {
			if r.(map[string]any)["kind"] != "source-map" {
				kept = append(kept, r)
			}
		}
		proof["byte_scan_ledger"] = kept
	})
	assertRefused(t, raw, "no row for the \"source-map\" layer")
}

func TestAByteScanLayerUsingTheSentinelAsItsOwnProbeIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "byte_scan_ledger")
		rows[0].(map[string]any)["probe"] = "PALAI-ENV-SENTINEL-c7f1a94e2b3d-DO-NOT-LEAK"
	})
	assertRefused(t, raw, "as its probe")
}

func TestARepairedLedgerRowWithNoReObservationIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "divergence_repair_ledger")
		// Repaired on a source read — the exact move that put the false sentences in the ledger in the first
		// place, and one this gate must refuse to accept as a repair.
		rows[0].(map[string]any)["reobserved"] = ""
	})
	assertRefused(t, raw, "carry a re-observation")
}

func TestAQueueThatNeverRefusedAStaleBindingIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "approval_ledger")
		kept := []any{}
		for _, r := range rows {
			if r.(map[string]any)["request_hash_matched"] == true {
				kept = append(kept, r)
			}
		}
		proof["approval_ledger"] = kept
		proof["approvals_refused_on_a_request_hash_mismatch"] = 0
	})
	assertRefused(t, raw, "REFUSED on a request-hash mismatch")
}

func TestAQueueThatDecidedNothingIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := ledgerRows(t, proof, "approval_ledger")
		for _, r := range rows {
			r.(map[string]any)["applied"] = false
		}
		proof["approvals_decided_from_the_console"] = 0
	})
	assertRefused(t, raw, "APPLIED")
}

// --- family 3: the manifest tries to vote on its own numbers ----------------------------------------------

func TestADeclaredCountThatDisagreesWithTheLedgerIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name, field string
		value       any
		want        string
	}{
		{"relay methods", "relay_exported_methods", 99, "the counts come from the rows"},
		{"gated methods", "relay_methods_behind_the_identity_gate", 99, "the counts come from the rows"},
		{"declared routes", "declared_routes", 99, "the counts come from the rows"},
		{"runbook steps", "runbook_steps_on_the_public_api", 99, "the count comes from the rows"},
		{"approvals applied", "approvals_decided_from_the_console", 99, "the counts come from the rows"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := mutate(t, func(proof map[string]any) { proof[tc.field] = tc.value })
			assertRefused(t, raw, tc.want)
		})
	}
}

func TestAnEditedContractLedgerIsRefused(t *testing.T) {
	raw := mutate(t, func(proof map[string]any) {
		rows := proof["contracts"].([]any)
		// Drop the row that names the cost of this epic's accessibility ceiling. The digest is left alone, so
		// only a gate that recomputes it from the CODE table can see this.
		proof["contracts"] = rows[:len(rows)-1]
	})
	assertRefused(t, raw, "no COMPLETE AdminConsoleProof")
}

// TestTheAdminConsoleLedgerRefusesTheUnconfirmedRow pins §3.5's own rule, for the fifth time this tree has had
// to write it: A VENDOR SILENCE IS NOT A DESIGN FREEDOM. N19 — whether a browser offers to SAVE a lone
// `type="password"` field outside a login form, and whether `new-password` suppresses that offer — could not
// be read on any published page, so it is UNCONFIRMED and enters no test, no doc sentence and no bundle field.
// It lives in known-gaps-1.0.md as CON-P5 and is measured by a manual-observation leg.
func TestTheAdminConsoleLedgerRefusesTheUnconfirmedRow(t *testing.T) {
	for _, req := range uat.AdminConsoleContracts {
		if req.Divergence == "N19" {
			t.Errorf("the canonical contract ledger carries N19, which §3.5 marks UNCONFIRMED: %s", req.Requirement)
		}
		if strings.Contains(strings.ToLower(req.Requirement), "suppress") &&
			strings.Contains(strings.ToLower(req.Requirement), "password manager") {
			t.Errorf("%s claims a password manager's save offer is suppressed — nothing published says so, and T4 gave up that claim rather than the design", req.Divergence)
		}
		if req.Divergence == "" || req.SourceURL == "" || strings.TrimSpace(req.Requirement) == "" {
			t.Errorf("a contract row is missing its §3.5 id, its source or its requirement: %+v", req)
		}
	}
}

// TestNoTierAdvancesInThisRelease is the E25 half of the standing rule, and it is asserted against the SHIPPED
// capability map rather than against a sentence in a script. `console` closes PREVIEW, and it closes PREVIEW in
// the epic that made it bigger — an identity gate, a secret-writing form, an approval decision surface, nine
// new pages and eight new /v1 routes. A tier must not rise in the epic that widened what it covers.
func TestNoTierAdvancesInThisRelease(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	// The composed gate underneath does the cross-bundle tier comparison against the committed
	// extensions-0.1.0 baseline; this asserts the composition actually happened, by proving a bundle that
	// ADVANCED a tier is refused THROUGH this gate rather than only through E23's.
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if refusals := uat.AdminConsolePromoteGate(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("control failed — the honest bundle must pass before an advanced tier can be shown to fail: %v", refusals)
	}
	m["maturity"] = "stable"
	advanced, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if refusals := uat.AdminConsolePromoteGate(advanced, "stable"); len(refusals) == 0 {
		t.Fatal("a STABLE-maturity admin-console bundle passed the gate at stable — the composed tier rules are not being reached from here")
	}
}

func assertRefused(t *testing.T, raw []byte, want string) {
	t.Helper()
	refusals := uat.AdminConsolePromoteGate(raw, "rc")
	if len(refusals) == 0 {
		t.Fatalf("the mutated bundle PASSED the gate — the counter it moved is read from the manifest rather than re-derived from the rows (want a refusal mentioning %q)", want)
	}
	if !refusalMentions(refusals, want) {
		t.Errorf("the mutated bundle was refused, but not for the reason the mutation created (want a refusal mentioning %q):\n%s", want, strings.Join(refusalDetails(refusals), "\n---\n"))
	}
}

func refusalMentions(refusals []uat.Refusal, substr string) bool {
	for _, r := range refusals {
		if strings.Contains(r.Detail, substr) {
			return true
		}
	}
	return false
}

func refusalDetails(refusals []uat.Refusal) []string {
	out := make([]string, 0, len(refusals))
	for _, r := range refusals {
		out = append(out, r.Detail)
	}
	return out
}
