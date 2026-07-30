package fleet

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE E24 REFUSAL MATRIX. Every negative here is a SHAPE-CONSISTENT manifest — it decodes, it verifies its
// own fields, its checksums recompute — that claims a property it did not earn. A gate that has never refused
// is not a gate, so each row is written as "this exact edit must be caught, for this exact reason", and the
// reason string is asserted rather than just the refusal count: a negative that fails for an unrelated reason
// proves the gate can fail, not that it can DISCRIMINATE.
//
// AND THE PROOF THAT EACH RECOMPUTE IS LOAD-BEARING IS HERE RATHER THAN IN A COMMENT. Every counter this
// bundle certifies is re-derived from bytes; the way to show a re-derivation is doing work is to move the
// BYTES while leaving the DECLARED counter at zero and watch the gate refuse anyway. Every mutation below
// does exactly that — each one leaves the proof's own number untouched — which is what distinguishes these
// from tests that merely edit a field the gate reads.
//
// THE MATRIX HAS TWO HALVES AND BOTH ARE NECESSARY. The first mutates a ledger so a guard FIRES (a wrong-pool
// offer appears, a run dead-letters, a machine drops after a revocation). The second DELETES the
// demonstration a zero rests on (the wrong-pool candidate, the parked run, the post-revocation renewal) and
// asserts the gate refuses the now-VACUOUS zero. A gate that only had the first half would pass a corpus
// where nothing could ever have gone wrong, which is the failure mode this whole family exists to refuse.

// committed reads the committed bundle as a mutable tree.
func committed(t *testing.T) map[string]any {
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

// fleetCaseOf returns the anchor case and its proof body from a decoded manifest.
func fleetCaseOf(t *testing.T, m map[string]any) (kase, proof map[string]any) {
	t.Helper()
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] == fleetAnchorCaseID {
			body, _ := entry["fleet_proof"].(map[string]any)
			if body == nil {
				t.Fatalf("%s carries no fleet_proof to tamper with", fleetAnchorCaseID)
			}
			return entry, body
		}
	}
	t.Fatalf("the committed bundle carries no %s anchor", fleetAnchorCaseID)
	return nil, nil
}

func marshal(t *testing.T, m map[string]any) []byte {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("re-encode the tampered manifest: %v", err)
	}
	return raw
}

func refusalsMention(refusals []uat.Refusal, substr string) bool {
	for _, r := range refusals {
		if strings.Contains(r.Detail, substr) {
			return true
		}
	}
	return false
}

// ledgerOf decodes one of the proof's carried ledgers into a mutable list. The ledgers ride as JSON STRINGS
// inside the manifest (json.RawMessage marshals to the raw document, so after a round-trip they are already
// decoded lists) — this helper hides which, so a row edit reads the same for every group.
func ledgerOf(t *testing.T, proof map[string]any, field string) []any {
	t.Helper()
	switch v := proof[field].(type) {
	case []any:
		return v
	case string:
		var rows []any
		if err := json.Unmarshal([]byte(v), &rows); err != nil {
			t.Fatalf("decode the %s: %v", field, err)
		}
		return rows
	default:
		t.Fatalf("the honest proof carries no %s to tamper with: %T", field, proof[field])
		return nil
	}
}

// setLedger writes a mutated ledger back in the shape the proof's field expects.
func setLedger(t *testing.T, proof map[string]any, field string, rows []any) {
	t.Helper()
	proof[field] = rows
}

// rowsWithout returns the ledger minus every row for which drop reports true, and fails if it removed
// nothing — a mutation that changed no bytes proves nothing about the gate.
func rowsWithout(t *testing.T, rows []any, what string, drop func(map[string]any) bool) []any {
	t.Helper()
	out := make([]any, 0, len(rows))
	removed := 0
	for _, r := range rows {
		row := r.(map[string]any)
		if drop(row) {
			removed++
			continue
		}
		out = append(out, r)
	}
	if removed == 0 {
		t.Fatalf("the mutation removed no %s row, so this negative would pass over an unchanged ledger", what)
	}
	return out
}

// --- the load-bearing pair ------------------------------------------------------------------------------

// TestFleetPromoteGateRefusesAndPasses is the load-bearing pair: the honest bundle PASSES at rc, and a
// promote BEYOND rc is REFUSED — because `workspaces` caps by construction on §6 leg 1, and adding a PLANE is
// not evidence the plane works in a real fleet.
func TestFleetPromoteGateRefusesAndPasses(t *testing.T) {
	raw := marshal(t, committed(t))

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the honest fleet bundle was refused at rc: %v", refusals)
	}
	stable := uat.PromoteGateFor(raw, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE passed — `workspaces` caps on §6 leg 1 and this release ran on ONE box with a structurally fake peer, on darwin/arm64, with no remote execution at all")
	}
	t.Logf("promote-stable refused for %d reason(s); first: %s", len(stable), stable[0])
}

// TestPromoteGateForRoutesToTheFleetFamily pins the dispatch, and this bundle is now the hardest case in the
// tree to dispatch correctly: it ALSO carries the E23 tool-approval claim, the E22 code-and-ship claim, the
// E21 tools-memory claim, the E20 agent-surface claim, the E19 wiring claim and E17 area claims, because it
// DERIVES its case set from those releases.
//
// The discriminating half is the SECOND assertion: with the fleet CLAIM MARKER dropped the manifest still
// routes here (recognition is by case id) and is refused for the missing proof — rather than sliding to
// ToolApprovalPromoteGate, which knows nothing about placement and would pass it. That is the
// promote-gate-family-dispatch defect, and dropping a claim marker is exactly how a release reaches it.
func TestPromoteGateForRoutesToTheFleetFamily(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	if proof["peer"] != uat.FleetPeer {
		t.Fatalf("the honest proof's peer is %v, want the structural %q", proof["peer"], uat.FleetPeer)
	}

	m = committed(t)
	kase, _ := fleetCaseOf(t, m)
	delete(kase, "fleet_claim")
	delete(kase, "fleet_proof")
	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a manifest carrying the E24 case ids with its fleet claim DROPPED passed — that is the promote-gate-family-dispatch defect: recognition must be by CASE ID, or dropping the marker reroutes the release to a weaker gate that passes it")
	}
	if !refusalsMention(refusals, "no COMPLETE FleetProof") {
		t.Errorf("the refusal does not name the missing fleet proof, so the release may have been refused by a DIFFERENT family's rule: %v", refusals)
	}
}

// --- (a) the wrong-pool offer ---------------------------------------------------------------------------

// TestAWrongPoolOfferIsRefusedAlthoughTheCounterSaysZero is the (a) recompute's mutation proof: one row's
// `offered` flips from false to true while `offers_to_the_wrong_pool` stays 0. If the gate read the field the
// bundle would pass.
func TestAWrongPoolOfferIsRefusedAlthoughTheCounterSaysZero(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "offer_ledger")
	flipped := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["attempt_pool_id"] != row["runner_pool_id"] && row["offered"] == false {
			row["offered"] = true
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("no wrong-pool candidate row to flip — the honest ledger must contain one, or the zero it certifies is vacuous")
	}
	setLedger(t, proof, "offer_ledger", rows)
	if proof["offers_to_the_wrong_pool"] != float64(0) {
		t.Fatalf("the mutation changed the DECLARED counter (%v); the whole point is that it stays 0", proof["offers_to_the_wrong_pool"])
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("an attempt OFFERED a machine in another pool passed the gate while the proof declared zero — the count is read off the manifest, not re-derived")
	}
	if !refusalsMention(refusals, "OFFERED a machine in another pool") {
		t.Errorf("the refusal does not name the wrong-pool offer: %v", refusals)
	}
}

// TestAnOfferLedgerWithNoWrongPoolCandidateIsRefused is the OTHER half, and the one a reader is likelier to
// dismiss: deleting the machine that was passed over makes the zero true and meaningless at once. The tree
// really was in this shape — one unbuffered `available` channel with no pool on either side — so a corpus
// that never presented a wrong machine is not a hypothetical.
func TestAnOfferLedgerWithNoWrongPoolCandidateIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "offer_ledger")
	setLedger(t, proof, "offer_ledger", rowsWithout(t, rows, "wrong-pool", func(row map[string]any) bool {
		return row["attempt_pool_id"] != row["runner_pool_id"]
	}))

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("an offer ledger with NO wrong-pool candidate in it passed — \"zero wrong-pool offers\" over a corpus that never had a wrong machine certifies nothing")
	}
	if !refusalsMention(refusals, "PASSED OVER for its pool") {
		t.Errorf("the refusal does not name the missing refusal demonstration: %v", refusals)
	}
}

// --- (b) the cross-tenant offer -------------------------------------------------------------------------

// TestACrossTenantOfferIsRefusedAlthoughTheCounterSaysZero is (b)'s mutation proof, and it is the counter
// that closes an actual hole rather than a hypothetical one: before this epic the runner plane carried no
// tenant at ALL, so any enrolled machine could take any tenant's attempt.
func TestACrossTenantOfferIsRefusedAlthoughTheCounterSaysZero(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "offer_ledger")
	flipped := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["attempt_organization_id"] != row["runner_organization_id"] && row["offered"] == false {
			row["offered"] = true
			flipped = true
			break
		}
	}
	if !flipped {
		t.Fatal("no foreign-tenant candidate row to flip — the honest ledger must contain one")
	}
	setLedger(t, proof, "offer_ledger", rows)
	if proof["offers_across_tenants"] != float64(0) {
		t.Fatalf("the mutation changed the DECLARED counter (%v)", proof["offers_across_tenants"])
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("an attempt offered ANOTHER TENANT's machine passed the gate while the proof declared zero")
	}
	if !refusalsMention(refusals, "crossed a TENANT boundary") {
		t.Errorf("the refusal does not name the cross-tenant offer: %v", refusals)
	}
}

// --- (c) the capacity death -----------------------------------------------------------------------------

// TestARunDeadLetteredForAbsentCapacityIsRefused is (c)'s mutation proof: the parked run's state becomes
// `dead_letter` while the declared count stays 0. That state is the pre-E24 behaviour — a 20-second dial over
// five attempts against a Mac host AWS documents as taking 6 to 20 minutes to boot.
func TestARunDeadLetteredForAbsentCapacityIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "run_ledger")
	killed := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["capacity_present"] == false && row["state"] == "waiting" {
			row["state"] = "dead_letter"
			killed = true
			break
		}
	}
	if !killed {
		t.Fatal("no parked run to kill — the honest ledger must contain one")
	}
	setLedger(t, proof, "run_ledger", rows)
	if proof["runs_dead_lettered_for_absent_capacity"] != float64(0) {
		t.Fatalf("the mutation changed the DECLARED counter (%v)", proof["runs_dead_lettered_for_absent_capacity"])
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a run that DIED of an empty pool passed the gate while the proof declared zero")
	}
	if !refusalsMention(refusals, "DIED because the target pool held no machine") {
		t.Errorf("the refusal does not name the capacity death: %v", refusals)
	}
}

// TestARunLedgerWithNoWokenRunIsRefused is (c)'s vacuity half: with no woken run, parking is only a nicer
// way to hang, and the wake is the half with no prior art on this plane.
func TestARunLedgerWithNoWokenRunIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "run_ledger")
	setLedger(t, proof, "run_ledger", rowsWithout(t, rows, "woken", func(row map[string]any) bool {
		return row["woken_by_a_machine_joining"] == true
	}))

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a run ledger in which nothing was ever WOKEN passed — parking that never ends is not the repair this release claims")
	}
	if !refusalsMention(refusals, "no parked run that a machine JOINING its pool woke") {
		t.Errorf("the refusal does not name the missing wake: %v", refusals)
	}
}

// --- (d) the key-revocation fence, and this file's most important pair -----------------------------------

// TestARenewalRefusedAfterAKeyRevocationIsRefusedByTheGate is (d)'s mutation proof, and it is the negative
// this epic's cheapest security test exists for: one post-revocation renewal's outcome becomes "refused"
// while the declared drop count stays 0. That is the build a well-meaning reader would produce after
// deciding a revoke should also cut the connection — and this test is what tells them no.
func TestARenewalRefusedAfterAKeyRevocationIsRefusedByTheGate(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "credential_ledger")
	cut := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["kind"] == "renew" && row["outcome"] == "ok" && row["key_revoked_at"] != "" {
			row["outcome"] = "refused"
			cut = true
			break
		}
	}
	if !cut {
		t.Fatal("no post-revocation renewal to cut — the honest ledger must contain one, or the fence is vacuous")
	}
	setLedger(t, proof, "credential_ledger", rows)
	if proof["enrolled_runners_dropped_by_a_key_revocation"] != float64(0) {
		t.Fatalf("the mutation changed the DECLARED counter (%v)", proof["enrolled_runners_dropped_by_a_key_revocation"])
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a machine DROPPED by a key revocation passed the gate while the proof declared zero — revocation would have become a back door into SAN-011's machine-level hard stop")
	}
	if !refusalsMention(refusals, "DROPPED by a key revocation") {
		t.Errorf("the refusal does not name the dropped machine: %v", refusals)
	}
	if !refusalsMention(refusals, "INTENDING TO CUT THE CONNECTION") {
		t.Error("the refusal does not argue back at the reader who arrived here meaning to cut the connection on a revoke — that sentence IS the answer, and a refusal that only reports a number leaves them to make the change again")
	}
}

// TestACredentialLedgerWithNoPostRevocationRenewalIsRefused is (d)'s vacuity half, and it is the one that
// makes the counter a counter: with every post-revocation renewal deleted, "all of them succeeded" is a
// statement about no rows at all.
func TestACredentialLedgerWithNoPostRevocationRenewalIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "credential_ledger")
	kept := rowsWithout(t, rows, "post-revocation renewal", func(row map[string]any) bool {
		revoked, _ := row["key_revoked_at"].(string)
		at, _ := row["at"].(string)
		return row["kind"] == "renew" && revoked != "" && at > revoked
	})
	setLedger(t, proof, "credential_ledger", kept)
	// The declared count is deliberately left at its honest value, so this also proves the
	// declared-equals-derived clause: the gate refuses whether it reads the vacuity or the mismatch first.
	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a credential ledger with NO renewal after the revocation passed — the fence is a COUNTER, and a count over no rows certifies nothing")
	}
	if !refusalsMention(refusals, "no renewal AFTER the revocation instant") &&
		!refusalsMention(refusals, "re-derives") {
		t.Errorf("the refusal names neither the missing renewals nor the count mismatch: %v", refusals)
	}
}

// TestAnEnrolmentAdmittedOnARevokedKeyIsRefused is the third half of the revocation claim — the one that is
// a FEATURE rather than a fence. If a revoked key still enrolled, the revocation did nothing.
func TestAnEnrolmentAdmittedOnARevokedKeyIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "credential_ledger")
	admitted := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["kind"] == "enroll" && row["outcome"] == "refused" {
			row["outcome"] = "ok"
			admitted = true
			break
		}
	}
	if !admitted {
		t.Fatal("no refused enrolment to admit — the honest ledger must contain one")
	}
	setLedger(t, proof, "credential_ledger", rows)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("an enrolment ADMITTED on a revoked key passed the gate")
	}
	if !refusalsMention(refusals, "ADMITTED on a revoked key") {
		t.Errorf("the refusal does not name the admitted enrolment: %v", refusals)
	}
}

// --- (e) the machine revocation across a restart ---------------------------------------------------------

// TestARevokedMachineThatCameBackIsRefused is (e)'s mutation proof: the revoked machine reconnects to the
// second gateway generation while the declared count stays 0. `revoked` was a process-local atomic.Bool, so
// this is what the tree did before T5 — and with `restart: always` a VM reboot did it on a schedule.
func TestARevokedMachineThatCameBackIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "lifecycle_ledger")
	revived := false
	for _, r := range rows {
		row := r.(map[string]any)
		gen, _ := row["gateway_generation"].(float64)
		if row["action"] == "revoke" && gen >= 2 {
			row["reconnected"] = true
			row["took_a_lease"] = true
			revived = true
			break
		}
	}
	if !revived {
		t.Fatal("no post-restart revoked machine to revive — the honest ledger must contain one")
	}
	setLedger(t, proof, "lifecycle_ledger", rows)
	if proof["revoked_runners_that_came_back"] != float64(0) {
		t.Fatalf("the mutation changed the DECLARED counter (%v)", proof["revoked_runners_that_came_back"])
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a revoked machine that RECONNECTED after a control-plane restart passed the gate while the proof declared zero")
	}
	if !refusalsMention(refusals, "came BACK after a control-plane restart") {
		t.Errorf("the refusal does not name the returned machine: %v", refusals)
	}
}

// TestARestartThatServedNobodyIsRefused is (e)'s vacuity half, and it is the difference between a revocation
// and an outage: a gateway that refused everybody would satisfy "zero came back" perfectly.
func TestARestartThatServedNobodyIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "lifecycle_ledger")
	setLedger(t, proof, "lifecycle_ledger", rowsWithout(t, rows, "post-restart lease", func(row map[string]any) bool {
		gen, _ := row["gateway_generation"].(float64)
		return gen >= 2 && row["action"] != "revoke" && row["took_a_lease"] == true
	}))

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a restarted gateway that served NOBODY passed — that is an outage, and it is indistinguishable from a revocation unless an unrevoked machine is shown still working")
	}
	if !refusalsMention(refusals, "served NO unrevoked machine") {
		t.Errorf("the refusal does not name the missing served machine: %v", refusals)
	}
}

// --- (g) the inventory and who names it ------------------------------------------------------------------

// TestAClientChosenIdentityIsRefused is (g)'s mutation proof, and it targets the T1 VERIFICATION-PASS defect
// rather than the obvious one: the id keeps the server's `rnr_` prefix and the certificate names something
// else. Two id minters that never met produced exactly this, and because every later lookup goes through the
// SAN, `last_seen_at` could not advance for anybody.
func TestAClientChosenIdentityIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "registry_ledger")
	row := rows[0].(map[string]any)
	row["certificate_dns"] = "runner-local.runners.palai.internal"
	setLedger(t, proof, "registry_ledger", rows)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a registry row whose certificate names a DIFFERENT machine than its own id passed the gate — that is the divergence T1's verification pass found, and it is invisible to any check that only looks at the id's prefix")
	}
	if !refusalsMention(refusals, "do not carry a SERVER-minted identity") {
		t.Errorf("the refusal does not name the client-chosen identity: %v", refusals)
	}
}

// TestARegistryWithNoSharedLabelIsRefused is (g)'s vacuity half and the strong form of the whole claim. Two
// machines with two different labels getting two rows was ALREADY true before this epic; what was not is that
// two machines asking for the SAME name hold two identities. So the mutation renames one of the colliding
// labels and asserts the gate notices the claim has weakened.
func TestARegistryWithNoSharedLabelIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	rows := ledgerOf(t, proof, "registry_ledger")
	seen := map[string]int{}
	for _, r := range rows {
		seen[r.(map[string]any)["requested_label"].(string)]++
	}
	renamed := false
	for _, r := range rows {
		row := r.(map[string]any)
		if seen[row["requested_label"].(string)] > 1 {
			row["requested_label"] = "a-label-nobody-else-asked-for"
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatal("the honest registry carries no label two machines shared — that IS the claim, so its absence is the vacuous form")
	}
	setLedger(t, proof, "registry_ledger", rows)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a registry in which no two machines ever asked for the same name passed — `--scale runner=3` gives three machines ONE hardcoded label, and that is the case the server-mint claim exists for")
	}
	if !refusalsMention(refusals, "shared by two distinct identities") {
		t.Errorf("the refusal does not name the missing collision: %v", refusals)
	}
}

// TestADeclaredDistinctCountThatTheRowsDoNotSupportIsRefused closes the last read-off-the-field path in (g):
// the ledger is honest and the NUMBER is inflated.
func TestADeclaredDistinctCountThatTheRowsDoNotSupportIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	proof["distinct_runners"] = float64(99)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a proof declaring ninety-nine machines over a three-row registry passed — the count must come from the rows")
	}
	if !refusalsMention(refusals, "but the proof declares 99") {
		t.Errorf("the refusal does not name the inflated count: %v", refusals)
	}
}

// --- the structural ceilings -----------------------------------------------------------------------------

// TestARealPeerIsStructurallyRefused pins the anti-fabrication constraint. This bundle ran on ONE box with
// fake machines; a manifest that types "real" into that field is refused by the shape verifier, so the
// overclaim cannot be made by accident or on purpose.
func TestARealPeerIsStructurallyRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	proof["peer"] = "real"

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a fleet proof claiming a REAL peer passed — FleetPeer is structurally the literal \"fake\" precisely so §6 leg 1 cannot be claimed from this box")
	}
	findings := uat.VerifyManifest(marshal(t, m), nil)
	if len(findings) == 0 {
		t.Error("the shape verifier accepted a real peer too — the constraint must hold in the verifier and not only in the promote gate, because `make evidence-verify` is what an operator runs")
	}
}

// TestAShrunkenContractLedgerIsRefused pins the (h) anchor: a bundle cannot implement a surface while
// quietly dropping the §3.5 row that named its gap. Dropping one row also moves the checksum anchor, so this
// negative fails twice — which is the design.
func TestAShrunkenContractLedgerIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := fleetCaseOf(t, m)
	contracts := proof["contracts"].([]any)
	proof["contracts"] = contracts[:len(contracts)-1]

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a fleet proof with a SHRUNKEN contract ledger passed — the ledger is the record of which published requirement each decision answers, and a release that drops one has dropped the reason it made a choice")
	}
}

// TestTheFleetLedgerRefusesTheUnconfirmedRows is the honest-absence pin, and it is the E23 precedent applied:
// §3.5 P12 (Scaleway's claimed sub-minute start and its automatic-delete option) and P13 (the concurrency and
// ordering semantics of Anthropic's Environments Work endpoints) could not be confirmed on any published
// page. A ledger of "published requirements we implement" is the wrong home for two things nobody could read,
// and NEITHER entered the code as an assumption — T4's park carries no duration constant at all and T2 chose
// its own order explicitly. If somebody adds them later, this test is where they meet the argument.
func TestTheFleetLedgerRefusesTheUnconfirmedRows(t *testing.T) {
	for _, req := range uat.FleetContracts {
		switch req.Divergence {
		case "P12", "P13":
			t.Errorf("§3.5 %s is UNCONFIRMED and is in the canonical contract ledger — a requirement nobody could read on a published page is not a requirement this bundle implements; it belongs in docs/operations/known-gaps-1.0.md and is measured by a §6 operator leg", req.Divergence)
		}
		if req.Divergence == "" || req.SourceURL == "" || req.Requirement == "" {
			t.Errorf("a contract row is incomplete (%q/%q): a requirement with no §3.5 row is a claim nobody triaged, and one with no source is a claim nobody can check", req.Divergence, req.SourceURL)
		}
		if !strings.Contains(req.SourceURL, "fetched 20") && !strings.Contains(req.SourceURL, "MEASURED") &&
			!strings.Contains(req.SourceURL, "re-read 20") {
			t.Errorf("§3.5 %s carries no fetch date or MEASURED marker in its source (%q) — plan §T8 (h) requires the date, because a vendor page that changed after we read it is the failure this field exists to survive", req.Divergence, req.SourceURL)
		}
	}
}
