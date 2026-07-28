package codeandship

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/tests/uat"
)

// THE E22 REFUSAL MATRIX. Every negative here is a SHAPE-CONSISTENT manifest — it decodes, it verifies its
// own fields, its checksums recompute — that claims a property it did not earn. A gate that has never refused
// is not a gate, so each row is written as "this exact edit must be caught, for this exact reason", and the
// reason string is asserted rather than just the refusal count: a negative that fails for an unrelated reason
// proves the gate can fail, not that it can DISCRIMINATE.

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

// shipCaseOf returns the anchor case and its proof body from a decoded manifest.
func shipCaseOf(t *testing.T, m map[string]any) (kase, proof map[string]any) {
	t.Helper()
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] == shipAnchorCaseID {
			body, _ := entry["code_and_ship_proof"].(map[string]any)
			if body == nil {
				t.Fatalf("%s carries no code_and_ship_proof to tamper with", shipAnchorCaseID)
			}
			return entry, body
		}
	}
	t.Fatalf("the committed bundle carries no %s anchor", shipAnchorCaseID)
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

// ledgerOf returns the decoded publication ledger from a proof body, as a mutable list.
func ledgerOf(t *testing.T, proof map[string]any) []any {
	t.Helper()
	raw, ok := proof["publication_ledger"].([]any)
	if !ok {
		t.Fatalf("the honest proof carries no publication ledger to tamper with: %v", proof["publication_ledger"])
	}
	return raw
}

// TestCodeAndShipPromoteGateRefusesAndPasses is the load-bearing pair: the honest bundle PASSES at rc, and a
// promote BEYOND rc is REFUSED — because `slack` caps at preview by construction (§6 leg 1) and no amount of
// working code changes that.
func TestCodeAndShipPromoteGateRefusesAndPasses(t *testing.T) {
	raw := marshal(t, committed(t))

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the honest code-and-ship bundle was refused at rc: %v", refusals)
	}
	stable := uat.PromoteGateFor(raw, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE passed — `slack` caps at preview on §6 leg 1 and this release contacted no workspace, no GitHub App and no signing identity")
	}
	t.Logf("promote-stable refused for %d reason(s); first: %s", len(stable), stable[0])
}

// TestPromoteGateForRoutesToTheCodeAndShipFamily pins the dispatch, and this bundle is the hardest case in
// the tree to dispatch correctly: it ALSO carries the E21 tools-memory claim, the E20 agent-surface claim,
// the E19 wiring claim and E17 area claims, because it DERIVES its case set from those releases. Without an
// E22 clause ahead of all four it would reroute to the tools-memory gate, which knows nothing about the
// unapproved-publication sweep, the destination sweep or the typed-operation ceiling — and all three crown
// guards would be optional in practice.
//
// The discriminator is deliberately a claim NONE of those families recognizes: with the E22 anchor's proof
// body removed, the E22 gate refuses. If dispatch had fallen through to E21, the bundle would pass.
func TestPromoteGateForRoutesToTheCodeAndShipFamily(t *testing.T) {
	m := committed(t)
	kase, _ := shipCaseOf(t, m)
	delete(kase, "code_and_ship_proof")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle whose code-and-ship PROOF is gone passed the promote gate — dispatch fell through to a weaker family and all three crown guards are optional")
	}
	if !refusalsMention(refusals, "no COMPLETE CodeAndShipProof") {
		t.Fatalf("the refusal did not come from the code-and-ship gate, so the dispatch is wrong: %v", refusals)
	}
}

// TestDroppingTheClaimMarkerIsStillRefused is the other half of the dispatch rule, and it is the one this
// repository has shipped a defect against: the family must be recognized by something the gate does NOT
// enforce. Here the code_and_ship_claim marker itself is deleted — the manifest verifier catches it via the
// presence rule, and the promote gate STILL routes to E22 because the family is keyed on the E22 CASE IDS.
func TestDroppingTheClaimMarkerIsStillRefused(t *testing.T) {
	m := committed(t)
	kase, _ := shipCaseOf(t, m)
	delete(kase, "code_and_ship_claim")
	delete(kase, "code_and_ship_proof")
	raw := marshal(t, m)

	findings := uat.VerifyManifest(raw, nil)
	if !findingsMention(findings, "code_and_ship_claim") {
		t.Fatalf("a manifest carrying the E22 cases with NO code-and-ship anchor verified without complaint: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "no COMPLETE CodeAndShipProof") {
		t.Fatalf("dropping the claim MARKER rerouted the bundle to a weaker family — the marker must not be what selects the gate that enforces it: %v", refusals)
	}
}

// TestAPublicationPublishedWithoutAnApproveIsRefused is the crown negative of this epic. The proof still SAYS
// zero; the ledger says otherwise, and the bytes win — in the shape verifier and again, independently, in the
// promote gate.
//
// The mutation is the one that matters most in the whole release: the DENIED publication is marked published.
// Every other failure in this chain announces itself — a refused click answers, a failed push warns, an
// unwired publisher logs — while an unapproved push is simply a branch appearing on somebody's remote.
func TestAPublicationPublishedWithoutAnApproveIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	rows := ledgerOf(t, proof)
	flipped := false
	for _, r := range rows {
		row := r.(map[string]any)
		if row["decision"] == "denied" {
			row["published"] = true
			flipped = true
		}
	}
	if !flipped {
		t.Fatal("the honest ledger carries no denied publication to flip — this negative has nothing to prove")
	}
	// The declared count is left at ZERO on purpose: this is the fabrication, not an oversight.
	if got := proof["published_without_approval"]; got != float64(0) {
		t.Fatalf("the honest proof declares %v unapproved publications; this negative needs it at 0", got)
	}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("a run that published a DENIED publication verified clean: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "reached a remote WITHOUT an approval") {
		t.Fatalf("the promote gate did not re-derive the unapproved publication from the ledger: %v", refusals)
	}
}

// TestALedgerWithNothingApprovedIsRefused: a zero over a run nobody ever approved certifies nothing. That is
// the vacuous form of the crown claim and it is refused — the ledger must show that an APPROVE is what
// publishes before "nothing published without one" is worth anything.
func TestALedgerWithNothingApprovedIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	kept := []any{}
	for _, r := range ledgerOf(t, proof) {
		row := r.(map[string]any)
		if row["published"] == true {
			continue
		}
		kept = append(kept, row)
	}
	proof["publication_ledger"] = kept
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("a proof over a ledger where nothing was ever approved verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "no publication that an APPROVE actually published") {
		t.Fatalf("the promote gate did not refuse the vacuous ledger for being vacuous: %v", refusals)
	}
}

// TestALedgerWithNoDenialIsRefused is the second vacuity guard, and it is a distinct claim: deny must PREVENT
// the side effect rather than record a verdict about it, and a ledger with no deny in it never demonstrated
// that.
func TestALedgerWithNoDenialIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	kept := []any{}
	for _, r := range ledgerOf(t, proof) {
		row := r.(map[string]any)
		if row["decision"] == "denied" {
			continue
		}
		kept = append(kept, row)
	}
	proof["publication_ledger"] = kept
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("a proof over a ledger with no denial verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "no DENIED publication that was withheld") {
		t.Fatalf("the promote gate did not refuse a ledger that never showed a deny: %v", refusals)
	}
}

// TestADestinationFieldTheModelCouldFillIsRefused is the plan's explicit requirement: the schema guard must
// be SHOWN TO FAIL on a `base` property before it is trusted. A base the model can choose is a base the
// approver did not approve.
//
// It also proves the sweep matches by SUBSTRING rather than by an exact name somebody thought of: the second
// half adds `target_branch`, which is the same field with a different spelling.
func TestADestinationFieldTheModelCouldFillIsRefused(t *testing.T) {
	for _, field := range []string{"base", "target_branch", "head_ref", "remote_url", "repository"} {
		t.Run(field, func(t *testing.T) {
			m := committed(t)
			_, proof := shipCaseOf(t, m)
			var schemas map[string]any
			if err := json.Unmarshal([]byte(toJSON(t, proof["publish_tool_schemas"])), &schemas); err != nil {
				t.Fatalf("decode the carried schemas: %v", err)
			}
			pr := schemas["palai.publish.pull_request"].(map[string]any)
			props := pr["properties"].(map[string]any)
			props[field] = map[string]any{"type": "string", "description": "where to open it"}
			proof["publish_tool_schemas"] = schemas
			raw := marshal(t, m)

			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
				t.Errorf("a publish tool exposing a %q property verified clean: %v", field, findings)
			}
			refusals := uat.PromoteGateFor(raw, "rc")
			if !refusalsMention(refusals, "destination field(s) the MODEL could fill") {
				t.Errorf("the promote gate did not re-derive the %q destination field from the schema: %v", field, refusals)
			}
		})
	}
}

// TestSchemasThatAreNotThePublishToolsAreRefused: a zero destination-field count over an empty object is the
// easiest green in this file to fabricate, so the sweep must be over BOTH publish tools' actual schemas.
func TestSchemasThatAreNotThePublishToolsAreRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	proof["publish_tool_schemas"] = map[string]any{"palai.publish.push": map[string]any{"type": "object"}}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("a proof carrying only one publish tool's schema verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "does not carry BOTH publish tools") {
		t.Fatalf("the promote gate accepted a destination sweep over schemas that are not the publish tools': %v", refusals)
	}
}

// TestATicketBodyThatGainedAuthorityIsRefused: the Jira description demanded a tool, a tenant and a remote.
// If any of the three turns up in what the run actually advertised, targeted or resolved, the ticket bought
// something — and the proof cannot declare its way out of bytes it carries.
func TestATicketBodyThatGainedAuthorityIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, needle, where string }{
		{"the tool the ticket minted was advertised", uat.CodeAndShipTicketTool, "advertised_tools"},
		{"the run moved to the tenant the ticket named", uat.CodeAndShipTicketTenant, "run_target"},
		{"the remote the ticket named became the destination", uat.CodeAndShipTicketRemote, "resolved_publication_target"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := committed(t)
			_, proof := shipCaseOf(t, m)
			var surface map[string]any
			if err := json.Unmarshal([]byte(toJSON(t, proof["authority_surface"])), &surface); err != nil {
				t.Fatalf("decode the authority surface: %v", err)
			}
			switch existing := surface[tc.where].(type) {
			case []any:
				surface[tc.where] = append(existing, tc.needle)
			case map[string]any:
				existing["compromised"] = tc.needle
			default:
				t.Fatalf("%s is neither a list nor an object: %T", tc.where, surface[tc.where])
			}
			proof["authority_surface"] = surface
			raw := marshal(t, m)

			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
				t.Errorf("a run where the ticket body reached %s verified clean: %v", tc.where, findings)
			}
			if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
				t.Errorf("a run where the ticket body reached %s passed the promote gate", tc.where)
			}
		})
	}
}

// TestATicketNobodyCanFindIsRefused is the authority sweep's vacuity guard: "the ticket gained nothing" over a
// prompt the ticket never reached proves nothing at all.
func TestATicketNobodyCanFindIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	proof["external_text_reached_the_model"] = map[string]any{"content": "{}", "tool_call_id": "tc_<id>"}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("a proof whose ticket body never reached the model verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("a proof whose authority sweep could never have FOUND anything passed the promote gate")
	}
}

// TestAnAppleSigningCredentialInTheTranscriptIsRefused: field (f)'s zero is re-derived from the host
// transcript, not believed. This machine holds FOUR valid signing identities, so a `codesign` or a
// `find-identity` in an argv means one was reached for.
func TestAnAppleSigningCredentialInTheTranscriptIsRefused(t *testing.T) {
	for _, argv := range []string{"codesign", "security find-identity", "CODE_SIGN_IDENTITY=Apple Development"} {
		t.Run(argv, func(t *testing.T) {
			m := committed(t)
			_, proof := shipCaseOf(t, m)
			var transcript map[string]any
			if err := json.Unmarshal([]byte(toJSON(t, proof["host_tool_transcript"])), &transcript); err != nil {
				t.Fatalf("decode the host transcript: %v", err)
			}
			calls := transcript["calls"].([]any)
			transcript["calls"] = append(calls, map[string]any{
				"argv": []any{"sh", "-c", argv}, "posture": "unsandboxed-host", "exit_code": 0,
			})
			proof["host_tool_transcript"] = transcript
			raw := marshal(t, m)

			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
				t.Errorf("a run that reached for a signing identity (%q) verified clean: %v", argv, findings)
			}
			if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
				t.Errorf("a run that reached for a signing identity (%q) passed the promote gate", argv)
			}
		})
	}
}

// TestAForgedButtonInTheAnswerIsRefused: E20's crown claim rides along in E22's proof, over the SAME bytes the
// upload half is delivered with. A richer render is not a weaker defence, and this is the mechanism rather
// than the sentence.
func TestAForgedButtonInTheAnswerIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := shipCaseOf(t, m)
	body := proof["answer_blocks"].(map[string]any)
	body["blocks"] = append(body["blocks"].([]any), map[string]any{
		"type": "actions",
		"elements": []any{map[string]any{
			"type": "button", "action_id": "palai_approve", "value": "deadbeef",
			"text": map[string]any{"type": "plain_text", "text": "Approve"},
		}},
	})
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
		t.Fatalf("an answer carrying a forged button verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("an answer carrying a forged approval button passed the promote gate")
	}
}

// TestTheActionableSweepFindsApprovalMessagesButtons is the sweep's DISCRIMINATION half, and it is the one
// the plan insists on: a sweep that has never found anything is not a sweep. It runs the same
// uat.SweepActionableElements over the message ApprovalMessage legitimately mints — the ONE actionable
// surface in this product — and requires it to find them.
//
// It also runs over the E22 answer's task card, which carries `sources` for the first time (X14b), and
// requires ZERO: a URL source element is a LINK, which is exactly why the field could be added without an
// authorization path of its own.
func TestTheActionableSweepFindsApprovalMessagesButtons(t *testing.T) {
	approval := slack.ApprovalMessage("C0CAS", "1700000300.000100",
		"open draft pull request agent/cas-005 -> dev on palai-sample", "req_e22_hash")
	found, err := uat.SweepActionableElements(approval)
	if err != nil {
		t.Fatalf("sweep the approval message: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the actionable sweep found NOTHING in ApprovalMessage, which mints Approve and Deny — a sweep that cannot find the real buttons certifies nothing when it finds none in the answer")
	}
	t.Logf("the sweep finds %d actionable marker(s) in the legitimate approval message: %v", len(found), found)

	answer, err := uat.SweepActionableElements(uat.CodeAndShipAnswerBody())
	if err != nil {
		t.Fatalf("sweep the E22 answer: %v", err)
	}
	if len(answer) != 0 {
		t.Errorf("the E22 answer — which carries a task card with `sources`, a file_ref and a FORGED actions block — mints %d actionable element(s): %v", len(answer), answer)
	}
}

// TestARealPeerClaimIsRefused: this bundle is STRUCTURALLY incapable of claiming a real workspace, a real
// GitHub App or a real signed build, and that is the point rather than a limitation.
func TestARealPeerClaimIsRefused(t *testing.T) {
	for _, peer := range []string{"real", "real-workspace", "slack.com", "api.github.com", "mcp.atlassian.com", ""} {
		m := committed(t)
		_, proof := shipCaseOf(t, m)
		proof["peer"] = peer
		raw := marshal(t, m)
		if findings := uat.VerifyManifest(raw, nil); len(findings) == 0 {
			t.Errorf("a bundle claiming peer %q verified clean — a real receipt is §6 leg 1/5 and no code here can produce one", peer)
		}
		if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
			t.Errorf("a bundle claiming peer %q passed the promote gate", peer)
		}
	}
}

// TestACodeAndShipCounterThatWasNeverEarnedIsRefused sweeps the counters one at a time. Each row is a claim
// the journey would have had to make FALSELY, and each must be caught on its own — a matrix that only fires
// when several things are wrong at once cannot tell a reader which claim failed.
func TestACodeAndShipCounterThatWasNeverEarnedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutga func(proof map[string]any)
	}{
		{"a repository whose before and after trees are the SAME, so nothing was written", func(p map[string]any) {
			p["repo_tree_after"] = p["repo_tree_before"]
		}},
		{"no commit at all", func(p map[string]any) { p["commits_made"] = float64(0) }},
		{"a declared unapproved-publication count above zero", func(p map[string]any) {
			p["published_without_approval"] = float64(1)
		}},
		{"a declared model-chosen destination count above zero", func(p map[string]any) {
			p["model_chosen_destination_fields"] = float64(1)
		}},
		{"external text that DID gain authority", func(p map[string]any) {
			p["external_text_authority_gained"] = float64(1)
		}},
		{"no needles, so the authority sweep could never fire", func(p map[string]any) {
			p["external_text_needles"] = []any{}
		}},
		{"a shell posture that does not say what it is", func(p map[string]any) { p["shell_posture"] = "1" }},
		{"a second typed capability", func(p map[string]any) { p["typed_capabilities"] = float64(2) }},
		{"a typed ios operation", func(p map[string]any) {
			p["typed_operations"] = []any{"swift.build-check", "ios.build"}
		}},
		{"a worker catalog digest hand-written over the real one", func(p map[string]any) {
			p["worker_catalog_digest"] = "sha256:" + strings.Repeat("a", 64)
		}},
		{"a host with NO signing identity, so the zero is vacuous", func(p map[string]any) {
			p["signing_identities_on_the_host"] = float64(0)
		}},
		{"a declared signing-credential count above zero", func(p map[string]any) {
			p["signing_credentials_engaged"] = float64(1)
		}},
		{"no artifact uploaded, so the delivery claim is vacuous", func(p map[string]any) {
			p["artifacts_uploaded"] = float64(0)
		}},
		{"a declared actionable-element count above zero", func(p map[string]any) {
			p["actionable_elements_minted"] = float64(1)
		}},
		{"a contract ledger with a divergence row dropped", func(p map[string]any) {
			ledger := p["contracts"].([]any)
			p["contracts"] = ledger[:len(ledger)-1]
		}},
		{"a ledger digest hand-written over an edited ledger", func(p map[string]any) {
			p["contracts_digest"] = "sha256:" + strings.Repeat("a", 64)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := committed(t)
			_, proof := shipCaseOf(t, m)
			tc.mutga(proof)
			raw := marshal(t, m)
			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_proof is incomplete") {
				t.Errorf("%s verified clean: %v", tc.name, findings)
			}
			if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
				t.Errorf("%s passed the promote gate", tc.name)
			}
		})
	}
}

// TestTwoCodeAndShipClaimsAreRefused: the promote gate judges the FIRST proof while the verifier checks all of
// them, so a second fabricated anchor could ride behind an honest one. Refuse instead of picking.
func TestTwoCodeAndShipClaimsAreRefused(t *testing.T) {
	m := committed(t)
	kase, _ := shipCaseOf(t, m)
	clone := map[string]any{}
	for k, v := range kase {
		clone[k] = v
	}
	clone["id"] = shipAnchorCaseID + "-SECOND"
	m["cases"] = append(m["cases"].([]any), clone)
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "code_and_ship_claims") {
		t.Fatalf("two code-and-ship anchors in one manifest verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "code_and_ship_claims in one manifest") {
		t.Fatalf("two anchors passed the promote gate: %v", refusals)
	}
}

// TestATierThatADVANCEDIsRefused proves the composed clauses are live in THIS family: E22 inherits "no tier
// advances against the committed E17 baseline" through E21, E20 and E19 rather than restating it, so a bundle
// that recomputed `slack` to stable is refused here without this file owning a tier rule of its own.
func TestATierThatADVANCEDIsRefused(t *testing.T) {
	m := committed(t)
	moved := false
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] != tierAnchorCaseID {
			continue
		}
		proof, _ := entry["capability_tier_proof"].(map[string]any)
		if proof == nil {
			t.Fatalf("%s carries no capability_tier_proof", tierAnchorCaseID)
		}
		for _, d := range proof["capabilities"].([]any) {
			decl := d.(map[string]any)
			if decl["capability"] == "apple-build" {
				decl["declared_tier"] = "preview"
				moved = true
			}
		}
		if snapshot, _ := proof["snapshot"].(map[string]any); snapshot != nil {
			snapshot["apple-build"] = "preview"
		}
	}
	if !moved {
		t.Fatal("the bundle declares no apple-build tier to move — the negative has nothing to prove")
	}
	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle declaring `apple-build` PREVIEW passed the code-and-ship promote gate — the composed no-tier-advance clause is not running, and this is the exact tier the epic argued about")
	}
	t.Logf("a fabricated apple-build preview was refused for %d reason(s); first: %s", len(refusals), refusals[0])
}

// TestNoNewCapabilityWasOpenedAndAppleBuildStaysDisabled is the plan's explicit requirement, asserted
// mechanically rather than promised in prose.
func TestNoNewCapabilityWasOpenedAndAppleBuildStaysDisabled(t *testing.T) {
	for _, capability := range uat.CapabilityTierOrder {
		if capability == "ios" || capability == "xcode-simulator" || capability == "mac" {
			t.Fatalf("a %q capability was opened — E22's whole decision is that a Mac is a DEPLOYMENT rather than a typed capability, and adding a member to CapabilityTierOrder moves CapabilityClaimsDigest and reddens every case checksum in every committed bundle", capability)
		}
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	var m struct {
		Cases []struct {
			ID    string `json:"id"`
			Proof *struct {
				Snapshot map[string]string `json:"snapshot"`
			} `json:"capability_tier_proof"`
		} `json:"cases"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	for _, c := range m.Cases {
		if c.ID != tierAnchorCaseID || c.Proof == nil {
			continue
		}
		for capability, want := range map[string]string{
			"slack": "preview", "knowledge-vector": "disabled", "apple-build": "disabled",
		} {
			if got := c.Proof.Snapshot[capability]; got != want {
				t.Errorf("this release closes `%s` at %q, want %q — E22 REMOVES a security boundary, which is the least earned moment for a tier", capability, got, want)
			}
		}
		return
	}
	t.Fatalf("the bundle carries no %s tier snapshot to check", tierAnchorCaseID)
}

// TestWorkspacesIsNotATierAndItsCorrectWordIsAvailable is plan §3.6 D15, made mechanical. The E22 plan's own
// first draft said `workspaces` would "become visible as preview" — and that sentence was WRONG in a way
// worth pinning: `workspaces` is not in the UAT tier ledger at all, and the function that answers for it
// draws from no tier vocabulary. It returns "available" or "unavailable".
//
// A plan naming a tier VALUE wrongly is exactly why the deviation table exists, so the correction is asserted
// against the SOURCE rather than remembered: E22 gives `workspaces` a derived answer for the first time (a
// native deployment sets PALAI_WORKSPACE_ROOT, which no shipped deployment ever did — §3.6 D2), and that is
// not a promotion.
func TestWorkspacesIsNotATierAndItsCorrectWordIsAvailable(t *testing.T) {
	for _, capability := range uat.CapabilityTierOrder {
		if capability == "workspaces" {
			t.Fatal("`workspaces` was added to CapabilityTierOrder — it is not governed by the stable/preview/disabled vocabulary, and adding it moves CapabilityClaimsDigest and reddens every case checksum in every committed bundle")
		}
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "apps", "control-plane", "api", "capabilities.go"))
	if err != nil {
		t.Fatalf("read capabilities.go: %v", err)
	}
	body := string(raw)
	start := strings.Index(body, "func workspacesCapability()")
	if start < 0 {
		t.Fatal("workspacesCapability is gone — the answer E22 makes derivable for the first time has no owner")
	}
	fn := body[start:]
	if end := strings.Index(fn, "\n}"); end > 0 {
		fn = fn[:end]
	}
	for _, want := range []string{`"available"`, `"unavailable"`} {
		if !strings.Contains(fn, want) {
			t.Errorf("workspacesCapability no longer answers %s", want)
		}
	}
	for _, forbidden := range []string{`"stable"`, `"preview"`, `"disabled"`} {
		if strings.Contains(fn, forbidden) {
			t.Errorf("workspacesCapability answers %s — it draws from no tier vocabulary, and a plan that said otherwise is the §3.6 D15 row", forbidden)
		}
	}
}

// TestTheTierDecisionIsOnTheRecord is the plan's explicit requirement: the counter-argument for advancing
// `apple-build` is REAL — a real repository is cloned, a real Xcode compiles, a real simulator is driven —
// and it must be stated and refused in the committed evidence rather than silently ignored. All FOUR reasons
// must be legible in the manifest itself, to a reader who opens only that file.
func TestTheTierDecisionIsOnTheRecord(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	body := string(raw)
	for _, required := range []struct{ what, needle string }{
		{"the counter-argument is NAMED (a real Xcode compiles and a real PR opens)", "a real Xcode compiles"},
		{"reason 1 — E22 does not touch the workers package at all", "DOES NOT TOUCH THE `workers` PACKAGE"},
		{"reason 2 — §6 leg 1 is open and §6 leg 5 opens beside it", "NO CAPTURED RECEIPT"},
		{"reason 3 — the epic REMOVES a boundary, which is the strongest reason of the four", "REMOVES A SECURITY BOUNDARY"},
		{"reason 4 — the newest dependency is a third-party tool on private Apple APIs", "PRIVATE APIs"},
		{"the five things that would have had to be true to move it", "WHAT WOULD HAVE HAD TO BE TRUE"},
		{"the sentence in workers/types.go that was WRONG, and its correction", "Absence by construction, not absence by inventory"},
		{"the price of the deletion, stated where the deletion is claimed", "NO SANDBOX"},
		{"the egress backstop that went with it", "EGRESS BACKSTOP"},
		{"the operating rule that is the only mitigation there is", "different customers"},
		{"the bring-up defaults are NOT platform structure", "BRING-UP CONVENIENCE"},
		{"no migration was opened at all", "MIGRATION: NONE"},
	} {
		if !strings.Contains(body, required.needle) {
			t.Errorf("the bundle does not carry %s (looked for %q) — the plan requires the tier counter-argument and the epic's price be stated ON THE RECORD, not in a gate's comments", required.what, required.needle)
		}
	}
}

// TestTheE22UncertaintiesAreInKnownGaps is the other side of the ledger's X23 refusal: refusing three
// unmeasured things from a ledger of published requirements is only honest if they LANDED somewhere a reader
// meets them. An uncertainty's home is the triage table, not a code comment — so this checks they arrived, by
// CONTENT rather than by an id somebody could rename.
//
// The rows the plan names for this task ride the same rule, including the ones T1 and T2 already wrote: a
// gate that trusted them to still be there is exactly the documentation debt this table exists to prevent.
func TestTheE22UncertaintiesAreInKnownGaps(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "docs", "operations", "known-gaps-1.0.md"))
	if err != nil {
		t.Fatalf("read the triage table: %v", err)
	}
	doc := string(raw)
	for _, required := range []struct{ what, needle string }{
		{"T1 — there is NO SANDBOX in the native posture", "THERE IS NO SANDBOX"},
		{"T1 — the egress backstop is gone too", "EGRESS BACKSTOP IS GONE"},
		{"T2 — `simctl --set` cannot be enforced", "cannot be enforced at all"},
		{"the operating rule, verbatim", "different customers → different Macs"},
		{"X20's answer, with its date", "HOME` does not select the CoreSimulator device set"},
		{"X21's answer, with its date", "X21 IS MEASURED"},
		{"X23(a) — a bot token's maximum upload size", "maximum upload size"},
		{"X23(b) — whether `blocks` accepts a markdown block on the upload path", "completeUploadExternal`'s `blocks`"},
		{"X23(c) — whether a QuickTime recording plays inline", "plays inline"},
		{"no live progress stream for a long build", "NO LIVE PROGRESS"},
		{"`axe`'s private-API dependency", "PRIVATE APIs"},
		{"one connection = one repository", "ONE CONNECTION IS ONE REPOSITORY"},
		{"one Xcode version per Mac", "knows ONE active Xcode"},
		{"the measurement date this epic's rows share", "2026-07-28"},
	} {
		if !strings.Contains(doc, required.needle) {
			t.Errorf("the triage table does not carry %s (looked for %q) — an uncertainty's home is that table, not a code comment", required.what, required.needle)
		}
	}
	// And the count the E18 T10 gate machine-reads must be untouched by all of it: none of these rows is an
	// RC-blocker, and saying so here stops a future edit from quietly grading one down.
	if !strings.Contains(doc, "RC-BLOCKERS: 0") {
		t.Error("the machine-read RC-blocker count is no longer 0 — E22 added rows and NONE of them is a blocker; if that changed, it must be argued in the table rather than discovered here")
	}
}

// TestNoIOSOrJiraIdentifierExistsInShippedCode is the mechanical form of this epic's defining sentence, and
// it is the answer to the one thing a reader could most easily misread.
//
// E22 solved iOS by NOT TYPING IT and reached Jira through a generic MCP connection, so NEITHER may appear as
// a Go IDENTIFIER in shipped code: not a type, not a function, not a field, not a constant. `palai up`'s
// Slack-named helpers are BRING-UP CONVENIENCE and the platform surface underneath them is generic — MCP
// connections, repository bindings, publish tools and a host shell — and a CLI's defaults are not a coupling.
//
// The scan is over DECLARED NAMES rather than over file text on purpose. Comments explaining why Jira is not
// typed, and one operator-facing warning that names Jira as an EXAMPLE of what an agent cannot reach, are the
// honest form; a `JiraConnection` type or an `ios.build` operation is the thing this refuses.
func TestNoIOSOrJiraIdentifierExistsInShippedCode(t *testing.T) {
	forbidden := []string{"jira", "xcode", "simctl", "iphone", "swiftui", "applebuild"}
	root := repoRoot(t)
	scanned := 0
	for _, dir := range []string{"apps", "adapters", "packages", "cmd", "storage"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			fset := token.NewFileSet()
			file, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil // a file this scan cannot parse is not a place an identifier hides
			}
			scanned++
			rel, _ := filepath.Rel(root, path)
			ast.Inspect(file, func(n ast.Node) bool {
				ident, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				lower := strings.ToLower(ident.Name)
				for _, word := range forbidden {
					if strings.Contains(lower, word) {
						t.Errorf("%s:%d declares or references the identifier %q — E22's defining decision is that iOS is NOT TYPED and Jira is reached through a GENERIC MCP connection; a name like this is the coupling this release says does not exist",
							rel, fset.Position(ident.Pos()).Line, ident.Name)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}
	if scanned < 100 {
		t.Fatalf("the identifier scan read only %d shipped files — a sweep that walked almost nothing cannot certify an absence", scanned)
	}
	t.Logf("scanned %d shipped .go files for iOS/Jira identifiers", scanned)
}

// toJSON re-marshals a decoded sub-tree so a test can decode it into a typed shape.
func toJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("re-encode a proof field: %v", err)
	}
	return string(raw)
}
