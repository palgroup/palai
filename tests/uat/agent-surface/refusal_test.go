package agentsurface

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE E20 REFUSAL MATRIX. Every negative here is a SHAPE-CONSISTENT manifest — it decodes, it verifies its
// own fields, its checksums recompute — that claims a surface property it did not earn. A gate that has
// never refused is not a gate, so each row is written as "this exact edit must be caught, for this exact
// reason", and the reason string is asserted rather than just the refusal count: a negative that fails for
// an unrelated reason proves the gate can fail, not that it can DISCRIMINATE.

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

// surfaceCaseOf returns the anchor case and its proof body from a decoded manifest.
func surfaceCaseOf(t *testing.T, m map[string]any) (kase, proof map[string]any) {
	t.Helper()
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] == surfaceAnchorCaseID {
			body, _ := entry["agent_surface_proof"].(map[string]any)
			if body == nil {
				t.Fatalf("%s carries no agent_surface_proof to tamper with", surfaceAnchorCaseID)
			}
			return entry, body
		}
	}
	t.Fatalf("the committed bundle carries no %s anchor", surfaceAnchorCaseID)
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

// refusalsMention reports whether any refusal's detail contains substr.
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

// TestAgentSurfacePromoteGateRefusesAndPasses is the load-bearing pair: the honest bundle PASSES at rc, and
// a promote BEYOND rc is REFUSED — because `slack` caps at preview by construction (§6 leg 1) and no amount
// of surface changes that.
func TestAgentSurfacePromoteGateRefusesAndPasses(t *testing.T) {
	raw := marshal(t, committed(t))

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the honest agent-surface bundle was refused at rc: %v", refusals)
	}
	stable := uat.PromoteGateFor(raw, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE passed — `slack` caps at preview on §6 leg 1 and this release contacted no workspace")
	}
	t.Logf("promote-stable refused for %d reason(s); first: %s", len(stable), stable[0])
}

// TestPromoteGateForRoutesToTheAgentSurfaceFamily pins the dispatch. The E20 bundle also carries the E19
// wiring claim and E17 area claims, so without an E20 clause AHEAD of both it would reroute to a gate that
// knows nothing about the forgery derivation — and the crown guard would be optional in practice.
//
// The discriminator is deliberately a claim NEITHER of those families recognizes: with the E20 anchor's
// proof body removed, the E20 gate refuses. If dispatch had fallen through to E19, the bundle would pass.
func TestPromoteGateForRoutesToTheAgentSurfaceFamily(t *testing.T) {
	m := committed(t)
	kase, _ := surfaceCaseOf(t, m)
	delete(kase, "agent_surface_proof")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle whose agent-surface PROOF is gone passed the promote gate — dispatch fell through to a weaker family and the forgery guard is optional")
	}
	if !refusalsMention(refusals, "no COMPLETE SlackAgentSurfaceProof") {
		t.Fatalf("the refusal did not come from the agent-surface gate, so the dispatch is wrong: %v", refusals)
	}
}

// TestDroppingTheClaimMarkerIsStillRefused is the other half of the dispatch rule, and it is the one this
// repository has shipped a defect against: the family must be recognized by something the gate does NOT
// enforce. Here the agent_surface_claim marker itself is deleted — the manifest verifier catches it via the
// presence rule, and the promote gate STILL routes to E20 because the family is keyed on the E20 CASE IDS.
func TestDroppingTheClaimMarkerIsStillRefused(t *testing.T) {
	m := committed(t)
	kase, _ := surfaceCaseOf(t, m)
	delete(kase, "agent_surface_claim")
	delete(kase, "agent_surface_proof")
	raw := marshal(t, m)

	findings := uat.VerifyManifest(raw, nil)
	if !findingsMention(findings, "agent_surface_claim") {
		t.Fatalf("a manifest carrying the E20 cases with NO agent-surface anchor verified without complaint: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "no COMPLETE SlackAgentSurfaceProof") {
		t.Fatalf("dropping the claim MARKER rerouted the bundle to a weaker family — the marker must not be what selects the gate that enforces it: %v", refusals)
	}
}

// TestAForgedButtonInTheClosingBlocksIsRefused is the crown negative. The proof still SAYS zero; the bytes
// say otherwise, and the bytes win — in the shape verifier and again, independently, in the promote gate.
func TestAForgedButtonInTheClosingBlocksIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := surfaceCaseOf(t, m)
	blocks, ok := proof["closing_blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("the honest proof carries no closing blocks to tamper with: %v", proof["closing_blocks"])
	}
	proof["closing_blocks"] = append(blocks, map[string]any{
		"type": "actions",
		"elements": []any{map[string]any{
			"type": "button", "action_id": "palai_approve", "value": "deadbeef",
			"text": map[string]any{"type": "plain_text", "text": "Approve"},
		}},
	})
	// The declared count is left at ZERO on purpose: this is the fabrication, not an oversight.
	if got := proof["actionable_elements_outside_approval_builder"]; got != float64(0) {
		t.Fatalf("the honest proof declares %v actionable elements outside the builder; this negative needs it at 0", got)
	}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "agent_surface_proof is incomplete") {
		t.Fatalf("a closing message carrying a forged button verified clean: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "actionable element(s) minted outside interactions.go") {
		t.Fatalf("the promote gate did not re-derive the forgery from the bytes: %v", refusals)
	}
}

// TestAVacuousSweepIsRefused: a forgery count of zero means nothing if the sweep could never have found
// anything. Zero approval-builder mints is exactly that state, and it is refused.
func TestAVacuousSweepIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := surfaceCaseOf(t, m)
	proof["approval_builder_mints"] = 0
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "agent_surface_proof is incomplete") {
		t.Fatalf("a proof whose sweep never demonstrated it could FIND an element verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("a vacuous forgery sweep passed the promote gate")
	}
}

// TestARealPeerClaimIsRefused: this bundle is STRUCTURALLY incapable of claiming a real workspace, and that
// is the point rather than a limitation. It is the SlackMappingProof.Peer discipline, unchanged since E17.
func TestARealPeerClaimIsRefused(t *testing.T) {
	for _, peer := range []string{"real", "real-workspace", "slack.com", ""} {
		m := committed(t)
		_, proof := surfaceCaseOf(t, m)
		proof["peer"] = peer
		raw := marshal(t, m)
		if findings := uat.VerifyManifest(raw, nil); len(findings) == 0 {
			t.Errorf("a bundle claiming peer %q verified clean — a real-workspace receipt is §6 leg 1 and no code here can produce one", peer)
		}
		if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
			t.Errorf("a bundle claiming peer %q passed the promote gate", peer)
		}
	}
}

// TestASurfaceCounterThatWasNeverEarnedIsRefused sweeps the counters one at a time. Each row is a claim the
// journey would have had to make FALSELY, and each must be caught on its own — a matrix that only fires when
// several things are wrong at once cannot tell a reader which claim failed.
func TestASurfaceCounterThatWasNeverEarnedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutga func(proof map[string]any)
	}{
		{"a run that produced TWO visible messages", func(p map[string]any) { p["visible_messages"] = float64(4) }},
		{"a run with no visible message at all", func(p map[string]any) { p["visible_messages"] = float64(2) }},
		{"an entrance dropped from the canonical three", func(p map[string]any) {
			p["admission_entrances"] = []any{"http-callback", "socket-mode"}
		}},
		{"an admission reserved under some OTHER route", func(p map[string]any) {
			p["admission_route"] = "/v1/slack/panel"
		}},
		{"a run that never went through the shared Admit", func(p map[string]any) {
			p["admitted_through_shared_admit"] = float64(2)
		}},
		{"no duplicate delivery, so invariance was never tested", func(p map[string]any) {
			p["deliveries"] = float64(3)
		}},
		{"a context entity that DID gain authority", func(p map[string]any) {
			p["context_entities_granted_authority"] = float64(1)
		}},
		{"a context channel that WAS read (the confused-deputy primitive)", func(p map[string]any) {
			p["context_channel_reads"] = float64(1)
		}},
		{"no context described, so every zero above is vacuous", func(p map[string]any) {
			p["context_entities_described"] = float64(0)
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
			_, proof := surfaceCaseOf(t, m)
			tc.mutga(proof)
			raw := marshal(t, m)
			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "agent_surface_proof is incomplete") {
				t.Errorf("%s verified clean: %v", tc.name, findings)
			}
			if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
				t.Errorf("%s passed the promote gate", tc.name)
			}
		})
	}
}

// TestTwoSurfaceClaimsAreRefused: the promote gate judges the FIRST proof while the verifier checks all of
// them, so a second fabricated anchor could ride behind an honest one. Refuse instead of picking.
func TestTwoSurfaceClaimsAreRefused(t *testing.T) {
	m := committed(t)
	kase, _ := surfaceCaseOf(t, m)
	clone := map[string]any{}
	for k, v := range kase {
		clone[k] = v
	}
	clone["id"] = surfaceAnchorCaseID + "-SECOND"
	m["cases"] = append(m["cases"].([]any), clone)
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "agent_surface_claims") {
		t.Fatalf("two agent-surface anchors in one manifest verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "agent_surface_claims in one manifest") {
		t.Fatalf("two anchors passed the promote gate: %v", refusals)
	}
}

// TestATierThatADVANCEDIsRefused proves the composed wiring clause is live in THIS family: the E20 gate
// inherits "no tier advances against the committed E17 baseline" rather than restating it, so a bundle that
// recomputed `slack` to stable is refused here without this file owning a tier rule of its own.
//
// The tier is a FUNCTION of claim outcomes, so it is moved the only way it can be moved: by rewriting the
// declared tier in the carried-forward E17 table. The recompute catches the disagreement, and the wiring
// clause catches the advance.
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
			if decl["capability"] == "slack" {
				decl["declared_tier"] = "stable"
				moved = true
			}
		}
		snapshot, _ := proof["snapshot"].(map[string]any)
		if snapshot != nil {
			snapshot["slack"] = "stable"
		}
	}
	if !moved {
		t.Fatal("the bundle declares no slack tier to move — the negative has nothing to prove")
	}
	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle declaring `slack` STABLE passed the agent-surface promote gate — the composed no-tier-advance clause is not running")
	}
	t.Logf("a fabricated stable tier was refused for %d reason(s); first: %s", len(refusals), refusals[0])
}

// TestTheTierDecisionIsOnTheRecord is the plan's explicit requirement: the counter-argument for advancing
// `slack` is REAL, and it must be stated and refused in the committed evidence rather than silently ignored.
// All THREE reasons must be legible in the manifest itself, to a reader who opens only that file.
func TestTheTierDecisionIsOnTheRecord(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	body := string(raw)
	for _, required := range []struct{ what, needle string }{
		{"the counter-argument is NAMED (a real workspace is connected and a real run completed)", "a real run DID complete on 2026-07-26"},
		{"reason 1 — the run left no captured receipt and Peer is structurally fake", "NO CAPTURED RECEIPT"},
		{"reason 1 — what a real receipt would have to BE", "re-derive workspace id + event id + run id"},
		{"reason 2 — it demonstrated ONE claim, not the twelve", "demonstrated ONE claim"},
		{"reason 3 — E20 GROWS the surface, which is the least earned moment for a tier", "E20 GROWS the surface"},
		{"the contribution to the leg is stated: bigger and cheaper, not closed", "BIGGER and CHEAPER"},
		{"the one-command execution the leg now has", "make uat-agent-surface-live"},
	} {
		if !strings.Contains(body, required.needle) {
			t.Errorf("the bundle does not carry %s (looked for %q) — the plan requires the tier counter-argument be stated and refused ON THE RECORD, not in a gate's comments", required.what, required.needle)
		}
	}
}
