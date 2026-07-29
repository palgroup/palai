package toolapproval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE E23 REFUSAL MATRIX. Every negative here is a SHAPE-CONSISTENT manifest — it decodes, it verifies its
// own fields, its checksums recompute — that claims a property it did not earn. A gate that has never
// refused is not a gate, so each row is written as "this exact edit must be caught, for this exact reason",
// and the reason string is asserted rather than just the refusal count: a negative that fails for an
// unrelated reason proves the gate can fail, not that it can DISCRIMINATE.
//
// AND THE PROOF THAT EACH RECOMPUTE IS LOAD-BEARING IS HERE RATHER THAN IN A COMMENT. Every counter this
// bundle certifies is re-derived from bytes; the way to show a re-derivation is doing work is to move the
// BYTES while leaving the DECLARED counter at zero and watch the gate refuse anyway. Each mutation below
// does exactly that, which is why every one of them keeps `"…": 0` untouched in the proof body.

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

// approvalCaseOf returns the anchor case and its proof body from a decoded manifest.
func approvalCaseOf(t *testing.T, m map[string]any) (kase, proof map[string]any) {
	t.Helper()
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] == approvalAnchorCaseID {
			body, _ := entry["tool_approval_proof"].(map[string]any)
			if body == nil {
				t.Fatalf("%s carries no tool_approval_proof to tamper with", approvalAnchorCaseID)
			}
			return entry, body
		}
	}
	t.Fatalf("the committed bundle carries no %s anchor", approvalAnchorCaseID)
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

func findingsMention(findings []uat.Finding, substr string) bool {
	for _, f := range findings {
		if strings.Contains(f.Detail, substr) {
			return true
		}
	}
	return false
}

// listOf returns a decoded ledger from a proof body, as a mutable list.
func listOf(t *testing.T, proof map[string]any, field string) []any {
	t.Helper()
	raw, ok := proof[field].([]any)
	if !ok {
		t.Fatalf("the honest proof carries no %s to tamper with: %v", field, proof[field])
	}
	return raw
}

// TestToolApprovalPromoteGateRefusesAndPasses is the load-bearing pair: the honest bundle PASSES at rc, and a
// promote BEYOND rc is REFUSED — because `slack` caps at preview by construction (§6 leg 1), and installing
// a control is not evidence the control works in a real workspace.
func TestToolApprovalPromoteGateRefusesAndPasses(t *testing.T) {
	raw := marshal(t, committed(t))

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the honest tool-approval bundle was refused at rc: %v", refusals)
	}
	stable := uat.PromoteGateFor(raw, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE passed — `slack` caps at preview on §6 leg 1 and this release contacted no workspace, no Atlassian tenant and no GitHub App")
	}
	t.Logf("promote-stable refused for %d reason(s); first: %s", len(stable), stable[0])
}

// TestPromoteGateForRoutesToTheToolApprovalFamily pins the dispatch, and this bundle is now the hardest case
// in the tree to dispatch correctly: it ALSO carries the E22 code-and-ship claim, the E21 tools-memory
// claim, the E20 agent-surface claim, the E19 wiring claim and E17 area claims, because it DERIVES its case
// set from those releases. Without an E23 clause ahead of all five it would reroute to the code-and-ship
// gate, which knows nothing about the ungoverned-side-effect sweep, the screen-authorship fence, the
// park/expiry counts or the single-mint recompute — and all four crown guards would be optional in practice.
//
// The discriminator is deliberately a claim NONE of those families recognizes: with the E23 anchor's proof
// body removed, the E23 gate refuses. If dispatch had fallen through to E22, the bundle would pass.
func TestPromoteGateForRoutesToTheToolApprovalFamily(t *testing.T) {
	m := committed(t)
	kase, _ := approvalCaseOf(t, m)
	delete(kase, "tool_approval_proof")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "no COMPLETE ToolApprovalProof") {
		t.Fatalf("a bundle carrying E23 cases with NO tool-approval proof was not refused by the E23 gate — dispatch fell through to a weaker family: %v", refusals)
	}
}

// TestDroppingTheClaimMarkerStillRoutesHere is the promote-gate-family-dispatch rule, asserted rather than
// commented. The family is recognized by the E23 CASE IDS, never by the tool_approval_claim the gate
// ENFORCES — so deleting the claim marker must NOT reroute the bundle to a gate that does not know about it.
func TestDroppingTheClaimMarkerStillRoutesHere(t *testing.T) {
	m := committed(t)
	kase, _ := approvalCaseOf(t, m)
	delete(kase, "tool_approval_claim")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "no COMPLETE ToolApprovalProof") {
		t.Fatalf("dropping the tool_approval_claim rerouted the bundle to a gate that passed it — that is the promote-gate-family-dispatch defect, and it is reachable by DELETING the very claim this gate enforces: %v", refusals)
	}
	// And the manifest verifier says the same thing from its own side: the cases are here, so the anchor is
	// owed.
	if findings := uat.VerifyManifest(marshal(t, m), nil); !findingsMention(findings, "tool_approval_claim") {
		t.Fatalf("a manifest carrying E23 cases with no claim marker verified without complaint: %v", findings)
	}
}

// --- (a) the gate itself ---------------------------------------------------------------------------------

// TestAGatedCallThatRanWithoutAHumanIsRefused is this epic's crown negative. The ledger row is flipped to
// `executed` with no approve, and the DECLARED counter is left at zero — so only a re-derivation can catch
// it.
func TestAGatedCallThatRanWithoutAHumanIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "call_ledger")
	for _, r := range rows {
		row := r.(map[string]any)
		if row["decision"] == "denied" {
			row["executed"] = true // the deny happened and the effect happened anyway
		}
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "RAN with no human decision") {
		t.Fatalf("a DENIED tool call marked as executed was not refused — the side-effect count is being read off the manifest instead of re-derived: %v", refusals)
	}
}

// TestALedgerWithNoExpiryIsRefused is the non-vacuity half nobody would think to write, and it is the one
// that catches an epic quietly dropping the expiry work: a corpus where every question was answered proves
// nothing about the question nobody answers.
func TestALedgerWithNoExpiryIsRefused(t *testing.T) {
	for _, tc := range []struct{ drop, want string }{
		{"expired", "contains no EXPIRED call"},
		{"denied", "contains no DENIED call"},
		{"approved", "contains no gated call an APPROVE actually ran"},
	} {
		m := committed(t)
		_, proof := approvalCaseOf(t, m)
		rows := listOf(t, proof, "call_ledger")
		kept := make([]any, 0, len(rows))
		for _, r := range rows {
			if r.(map[string]any)["decision"] == tc.drop {
				continue
			}
			kept = append(kept, r)
		}
		proof["call_ledger"] = kept

		refusals := uat.PromoteGateFor(marshal(t, m), "rc")
		if !refusalsMention(refusals, tc.want) {
			t.Errorf("a ledger with no %s row was accepted — the zero it certifies is then free: %v", tc.drop, refusals)
		}
	}
}

// TestALedgerWithNoUngatedCallIsRefused is the other direction, and it is the one that catches the OPPOSITE
// overreach: a build that gated everything would satisfy every negative above while breaking every
// deployment that configured nothing.
func TestALedgerWithNoUngatedCallIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "call_ledger")
	kept := make([]any, 0, len(rows))
	for _, r := range rows {
		if r.(map[string]any)["gated"] == false {
			continue
		}
		kept = append(kept, r)
	}
	proof["call_ledger"] = kept

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "contains no UNGATED call") {
		t.Fatalf("a ledger in which EVERY tool was gated was accepted — \"a tool that declares no approval is bit-unchanged\" is then unproven: %v", refusals)
	}
}

// --- (b) the fence ---------------------------------------------------------------------------------------

// TestTheServersDescriptionOnTheApprovalScreenIsRefused IS THE TEST THE PLAN CALLED THIS EPIC'S CHEAPEST.
// It puts the MCP server's own `description` into the rendered screen — exactly what a future reader
// proposing "let's also show the tool's description, it helps the user" would produce — and the gate must
// refuse, with the counter still declared zero.
func TestTheServersDescriptionOnTheApprovalScreenIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	screen, ok := proof["approval_screen"].(map[string]any)
	if !ok {
		t.Fatalf("the honest proof carries no approval screen to tamper with")
	}
	message := screen["message"].(map[string]any)
	// The helpful edit: the description, rendered beside the identity.
	message["text"] = message["text"].(string) + "\n" + uat.ToolApprovalPeerDescription

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "written by the MODEL or by the SERVER BEING CALLED") {
		t.Fatalf("the MCP server's own description reached the approval screen and the gate passed it — this is the ONE refusal this epic must never lose: %v", refusals)
	}
}

// TestTheModelsProseInTheModalIsRefused is the same fence on the other surface. Two surfaces render the same
// row, and a leak into only one of them is just as much of a breach — so the sweep runs over both.
func TestTheModelsProseInTheModalIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	screen := proof["approval_screen"].(map[string]any)
	modal := screen["modal"].(map[string]any)
	view := modal["view"].(map[string]any)
	view["private_metadata"] = view["private_metadata"].(string) + uat.ToolApprovalModelProse

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "written by the MODEL or by the SERVER BEING CALLED") {
		t.Fatalf("the model's prose reached the MODAL and the gate passed it — the fence must cover both surfaces, because they render the same row: %v", refusals)
	}
}

// TestUntrustedTextThatNeverArrivedMakesTheFenceVacuous is the fence's own non-vacuity guard: a zero over
// text nobody ever sent certifies nothing, so the needles must be findable in what ARRIVED FROM OUTSIDE.
func TestUntrustedTextThatNeverArrivedMakesTheFenceVacuous(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["untrusted_text_arrived"] = map[string]any{"tools_list_result": map[string]any{"name": "transitionJiraIssue"}}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "findable in what ARRIVED FROM OUTSIDE") {
		t.Fatalf("a fence over untrusted text that was never delivered passed — that is the vacuous form of the claim: %v", refusals)
	}
}

// TestAnEmptyApprovalScreenIsRefused catches the cheapest way to satisfy every negative above: show nothing.
// A screen with no identity, no operator label and no arguments has no forbidden characters on it either.
func TestAnEmptyApprovalScreenIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["approval_screen"] = map[string]any{
		"message": map[string]any{"channel": "C0E23", "text": "Approval requested",
			"blocks": []any{map[string]any{"type": "actions", "elements": []any{
				map[string]any{"type": "button", "action_id": "palai_approve", "value": "hash"}}}}},
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "shows neither the resolved identity") {
		t.Fatalf("an approval screen showing NOTHING passed the authorship fence — showing nothing is the cheapest way to show nothing forbidden, and it is the screen this epic exists to prevent: %v", refusals)
	}
}

// --- (c)+(d) the park and its deadline -------------------------------------------------------------------

// TestARunThatWentTerminalWhileWaitingIsRefused reproduces E22's measured defect inside the evidence and
// requires the gate to catch it: the run answered while a human still owed a decision.
func TestARunThatWentTerminalWhileWaitingIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	for _, r := range listOf(t, proof, "run_ledger") {
		row := r.(map[string]any)
		if row["approval"] == "pending" {
			row["state"] = "completed"
		}
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "went TERMINAL while a human still owed an answer") {
		t.Fatalf("a run that answered while its question was open passed — that is exactly the E22 defect this epic repairs: %v", refusals)
	}
}

// TestARunLeftWaitingAfterExpiryIsRefused is the half with no prior art in this tree: the approval expired,
// the call was cancelled, and the run was left parked forever.
func TestARunLeftWaitingAfterExpiryIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	for _, r := range listOf(t, proof, "run_ledger") {
		row := r.(map[string]any)
		if row["approval"] == "expired" {
			row["state"] = "waiting"
		}
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "still WAITING after their approval expired") {
		t.Fatalf("an expiry that cancelled the call and left the run parked forever passed — the worst state of a gate is not letting something through, it is holding a run open on a question nobody will answer: %v", refusals)
	}
}

// TestARunLedgerWithNothingParkedIsRefused is the park's non-vacuity half: over a corpus where nothing ever
// waited, both zeros above are free.
func TestARunLedgerWithNothingParkedIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "run_ledger")
	for _, r := range rows {
		r.(map[string]any)["parked_on_approval"] = false
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "no run actually PARKED") {
		t.Fatalf("a run ledger in which nothing ever parked was accepted: %v", refusals)
	}
}

// --- (e) who may decide ----------------------------------------------------------------------------------

// TestAnUnauthorizedDecisionThatAppliedIsRefused is T2's crown negative: a principal outside the project's
// approver list whose click nevertheless decided something.
func TestAnUnauthorizedDecisionThatAppliedIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	for _, r := range listOf(t, proof, "decision_ledger") {
		row := r.(map[string]any)
		if row["authorized"] == false && row["surface"] == "slack" {
			row["applied"] = true
		}
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "by an UNAUTHORIZED principal were applied") {
		t.Fatalf("an unlisted Slack user's click decided something and the gate passed it: %v", refusals)
	}
}

// TestADecisionLedgerRefusingOnlyOneSurfaceIsRefused is the structural half of T2's design: the check lives
// in ApplyApprovalDecision, the ONE throat both surfaces pass through. A ledger showing a refusal on only
// one of them is consistent with a guard bolted onto that caller — which is the shape the design refuses,
// because the next caller forgets it.
func TestADecisionLedgerRefusingOnlyOneSurfaceIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "decision_ledger")
	kept := make([]any, 0, len(rows))
	for _, r := range rows {
		row := r.(map[string]any)
		if row["authorized"] == false && row["surface"] == "http" {
			continue
		}
		kept = append(kept, r)
	}
	proof["decision_ledger"] = kept

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "must show BOTH surfaces refusing") {
		t.Fatalf("a decision ledger that only ever refused a Slack click was accepted — the HTTP surface is where the approver concept did not exist AT ALL before this epic: %v", refusals)
	}
}

// --- (f) the destination ---------------------------------------------------------------------------------

// TestAMergeToolThatTakesAPullRequestNumberIsRefused is the merge destination's negative, and it is the
// least recoverable one in the tree: a model-chosen pull request number would let an approved merge land on
// somebody else's pull request while the approval message still read like this run's.
func TestAMergeToolThatTakesAPullRequestNumberIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	schemas := proof["publish_tool_schemas"].(map[string]any)
	merge := schemas["palai.publish.merge_pull_request"].(map[string]any)
	merge["properties"] = map[string]any{"pull_request_number": map[string]any{"type": "integer"}}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "destination field(s) the MODEL could fill") {
		t.Fatalf("a merge tool exposing `pull_request_number` to the model passed — a destination the model can name is a destination the approver did not approve: %v", refusals)
	}
}

// TestASchemaSetMissingTheMergeToolIsRefused is the destination sweep's non-vacuity half: a zero over
// schemas that are not the publish tools' proves nothing about them.
func TestASchemaSetMissingTheMergeToolIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	schemas := proof["publish_tool_schemas"].(map[string]any)
	delete(schemas, "palai.publish.merge_pull_request")

	findings := uat.VerifyManifest(marshal(t, m), nil)
	if !findingsMention(findings, "tool_approval_proof is incomplete") {
		t.Fatalf("a proof carrying only two of the three publish tools' schemas verified clean: %v", findings)
	}
}

// --- (g) the single mint ---------------------------------------------------------------------------------

// TestAScreenWithNoButtonIsRefused pins the honest form of E20's claim. This bundle does NOT say "zero
// actionable elements" — the approval screen must HAVE buttons — so the negative is the empty one.
func TestAScreenWithNoButtonIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["approval_screen"] = map[string]any{"message": map[string]any{
		"text": "Approval requested: " + uat.ToolApprovalIdentity,
		"blocks": []any{map[string]any{"type": "markdown",
			"text": uat.ToolApprovalOperatorLabel + " PAL-42"}}}}
	proof["actionable_elements_minted"] = 0

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "carries NO actionable element") {
		t.Fatalf("an approval screen with nothing to press passed — a human cannot decide from it, and the sweeps that rest on it cannot discriminate: %v", refusals)
	}
}

// TestAFabricatedMintCountIsRefused moves the DECLARED count while leaving the bytes alone. It is the
// smallest possible demonstration that the count comes from the screen rather than from the manifest.
func TestAFabricatedMintCountIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["actionable_elements_minted"] = float64(2)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "but the proof declares") {
		t.Fatalf("a fabricated actionable-element count was accepted: %v", refusals)
	}
}

// TestAFabricatedMintFileListIsRefused is the same for the file list, which is recomputed from the SOURCE of
// two packages. It is the clause a reader meets if they move the modal's construction one package over.
func TestAFabricatedMintFileListIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["actionable_element_mint_files"] = []any{"adapters/integrations/slack/blocks.go"}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "the file list is re-derived from source") {
		t.Fatalf("a fabricated mint-file list was accepted — the list is recomputed from two packages' source, never read off the manifest: %v", refusals)
	}
}

// --- (i) the decision surface (E23 T8) -------------------------------------------------------------------

// TestAGatedCallNobodyCouldHaveDecidedIsRefused IS THE ROW THAT WOULD HAVE REDDENED THE RELEASE BEFORE THIS
// ONE, and that is the whole argument for it existing. Through T7 every gated non-publication call had NO
// production decision surface: the exit gate measured it, wrote HIL-P8, and tagged anyway — because a gap
// written in prose cannot refuse anything. This row is that prose turned into a derivation.
//
// The mutation removes the QUESTION from one row while leaving `gated_calls_with_no_decision_surface` at
// zero, which is exactly the shape of the shipped defect: the call is parked, the counter says all is well,
// and only the reaper ever touches it.
func TestAGatedCallNobodyCouldHaveDecidedIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "decision_surface_ledger")
	delete(rows[0].(map[string]any), "ask")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "had NO REACHABLE DECISION SURFACE") {
		t.Fatalf("a gated call nobody could have decided was accepted — it parks a run only the expiry reaper "+
			"can release, which is HIL-P8, the defect this epic's own exit gate found in itself: %v", refusals)
	}
}

// TestThePublicationScreenPostedForAToolCallIsRefused is the failure mode of getting the PUMP'S
// DISCRIMINATOR wrong, and it is the subtle one: E19's two-button `ApprovalMessage` is a perfectly valid
// screen with perfectly valid buttons bound to the right hash — it just shows a human NO ARGUMENTS. Every
// other check in this file passes over it. This one does not.
func TestThePublicationScreenPostedForAToolCallIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "decision_surface_ledger")
	row := rows[0].(map[string]any)
	// The publication screen, rendered by the SHIPPED renderer and bound to this very call's hash: only the
	// third button is missing, and with it every argument a human would have read.
	var publicationScreen map[string]any
	if err := json.Unmarshal(uat.ToolApprovalPublicationScreenFor(row["request_hash"].(string)), &publicationScreen); err != nil {
		t.Fatalf("render the publication screen: %v", err)
	}
	row["ask"] = publicationScreen

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "had NO REACHABLE DECISION SURFACE") {
		t.Fatalf("a TOOL call asked about with the two-button PUBLICATION screen was accepted — the buttons "+
			"work and the hash matches, but the human is shown no arguments at all: %v", refusals)
	}
}

// TestAnAskBoundToAnotherCallIsRefused moves the BINDING rather than the screen. The question is posted, it
// has three buttons, it is the right shape — and its value is another call's hash, so pressing it decides
// nothing here. `DecideToolApproval` refuses on exactly this mismatch; the gate must too, or the bundle
// could certify reachability with a corpus of buttons for the wrong calls.
func TestAnAskBoundToAnotherCallIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "decision_surface_ledger")
	rows[0].(map[string]any)["request_hash"] = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "had NO REACHABLE DECISION SURFACE") {
		t.Fatalf("a question whose buttons carry ANOTHER call's hash was accepted as a decision surface — "+
			"pressing it authorizes nothing for this call: %v", refusals)
	}
}

// TestADecisionSurfaceLedgerWithNothingDecidedIsRefused and its twin below are the two vacuity halves, and
// they pull in opposite directions on purpose. If nothing was ever decided, the asks may be screens nobody
// can press. If nothing was ever left unanswered, the counter has quietly become "everything got clicked" —
// which is not the claim, because an approval nobody presses is a CORRECT outcome this release already
// certifies (HIL-003: the reaper cancels the call and wakes the run).
func TestADecisionSurfaceLedgerWithNothingDecidedIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	for _, r := range listOf(t, proof, "decision_surface_ledger") {
		r.(map[string]any)["decided_by"] = ""
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "no gated call that was actually DECIDED") {
		t.Fatalf("a ledger in which nobody ever decided anything was accepted — the asks could be screens no "+
			"button on which does anything: %v", refusals)
	}
}

func TestADecisionSurfaceLedgerWithNothingUnansweredIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	for _, r := range listOf(t, proof, "decision_surface_ledger") {
		r.(map[string]any)["decided_by"] = "slack:T0E23:U0APPROVER"
	}

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "left UNANSWERED") {
		t.Fatalf("a ledger in which every question was answered was accepted — the counter is then measuring "+
			"enthusiasm rather than reachability: %v", refusals)
	}
}

// --- the anchor itself -----------------------------------------------------------------------------------

// TestAnUnfakePeerIsRefused pins the structural ceiling: this bundle cannot claim a real workspace, a real
// Atlassian tenant or a real merge, because the field will not hold the word.
func TestAnUnfakePeerIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	proof["peer"] = "real"

	findings := uat.VerifyManifest(marshal(t, m), nil)
	if !findingsMention(findings, "tool_approval_proof is incomplete") {
		t.Fatalf("a proof claiming a REAL peer verified clean — ToolApprovalPeer is structurally \"fake\" and that is the whole honest ceiling: %v", findings)
	}
}

// TestAShrunkenContractLedgerIsRefused pins the §3.5 anchor: dropping a vendor row would let an epic
// implement a surface while silently deleting the requirement that named its gap.
func TestAShrunkenContractLedgerIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := approvalCaseOf(t, m)
	rows := listOf(t, proof, "contracts")
	proof["contracts"] = rows[:len(rows)-1]

	findings := uat.VerifyManifest(marshal(t, m), nil)
	if !findingsMention(findings, "tool_approval_proof is incomplete") {
		t.Fatalf("a shrunken contract ledger verified clean: %v", findings)
	}
}

// TestASecondProofCannotRideBehindAnHonestOne pins "exactly one": the gate judges the FIRST proof while the
// verifier checks all of them, so a second could otherwise ride along.
func TestASecondProofCannotRideBehindAnHonestOne(t *testing.T) {
	m := committed(t)
	kase, _ := approvalCaseOf(t, m)
	clone := map[string]any{}
	for k, v := range kase {
		clone[k] = v
	}
	clone["id"] = approvalAnchorCaseID + "-SECOND"
	m["cases"] = append(m["cases"].([]any), clone)

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if !refusalsMention(refusals, "tool_approval_claims in one manifest") {
		t.Fatalf("a second tool-approval claim rode along unnoticed: %v", refusals)
	}
}
