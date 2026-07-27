package toolsmemory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// THE E21 REFUSAL MATRIX. Every negative here is a SHAPE-CONSISTENT manifest — it decodes, it verifies its
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

// memoryCaseOf returns the anchor case and its proof body from a decoded manifest.
func memoryCaseOf(t *testing.T, m map[string]any) (kase, proof map[string]any) {
	t.Helper()
	for _, c := range m["cases"].([]any) {
		entry := c.(map[string]any)
		if entry["id"] == memoryAnchorCaseID {
			body, _ := entry["tools_memory_proof"].(map[string]any)
			if body == nil {
				t.Fatalf("%s carries no tools_memory_proof to tamper with", memoryAnchorCaseID)
			}
			return entry, body
		}
	}
	t.Fatalf("the committed bundle carries no %s anchor", memoryAnchorCaseID)
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

// TestToolsMemoryPromoteGateRefusesAndPasses is the load-bearing pair: the honest bundle PASSES at rc, and a
// promote BEYOND rc is REFUSED — because `slack` caps at preview by construction (§6 leg 1) and no amount of
// working tools changes that.
func TestToolsMemoryPromoteGateRefusesAndPasses(t *testing.T) {
	raw := marshal(t, committed(t))

	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) != 0 {
		t.Fatalf("the honest tools-memory bundle was refused at rc: %v", refusals)
	}
	stable := uat.PromoteGateFor(raw, "stable")
	if len(stable) == 0 {
		t.Fatal("a promote to STABLE passed — `slack` caps at preview on §6 leg 1 and this release contacted no workspace and no MCP server")
	}
	t.Logf("promote-stable refused for %d reason(s); first: %s", len(stable), stable[0])
}

// TestPromoteGateForRoutesToTheToolsMemoryFamily pins the dispatch. The E21 bundle ALSO carries the E20
// agent-surface claim, the E19 wiring claim and E17 area claims, so without an E21 clause ahead of all three
// it would reroute to a gate that knows nothing about the stored-search-byte sweep — and the crown guard
// would be optional in practice.
//
// The discriminator is deliberately a claim NONE of those families recognizes: with the E21 anchor's proof
// body removed, the E21 gate refuses. If dispatch had fallen through to E20, the bundle would pass.
func TestPromoteGateForRoutesToTheToolsMemoryFamily(t *testing.T) {
	m := committed(t)
	kase, _ := memoryCaseOf(t, m)
	delete(kase, "tools_memory_proof")

	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle whose tools-memory PROOF is gone passed the promote gate — dispatch fell through to a weaker family and both crown guards are optional")
	}
	if !refusalsMention(refusals, "no COMPLETE ToolsMemoryProof") {
		t.Fatalf("the refusal did not come from the tools-memory gate, so the dispatch is wrong: %v", refusals)
	}
}

// TestDroppingTheClaimMarkerIsStillRefused is the other half of the dispatch rule, and it is the one this
// repository has shipped a defect against: the family must be recognized by something the gate does NOT
// enforce. Here the tools_memory_claim marker itself is deleted — the manifest verifier catches it via the
// presence rule, and the promote gate STILL routes to E21 because the family is keyed on the E21 CASE IDS.
func TestDroppingTheClaimMarkerIsStillRefused(t *testing.T) {
	m := committed(t)
	kase, _ := memoryCaseOf(t, m)
	delete(kase, "tools_memory_claim")
	delete(kase, "tools_memory_proof")
	raw := marshal(t, m)

	findings := uat.VerifyManifest(raw, nil)
	if !findingsMention(findings, "tools_memory_claim") {
		t.Fatalf("a manifest carrying the E21 cases with NO tools-memory anchor verified without complaint: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "no COMPLETE ToolsMemoryProof") {
		t.Fatalf("dropping the claim MARKER rerouted the bundle to a weaker family — the marker must not be what selects the gate that enforces it: %v", refusals)
	}
}

// TestAStoredSearchResultIsRefused is the crown negative, and it is the exact edit that would have made this
// bundle honest ABOUT A TREE THAT WAS NOT. The proof still SAYS zero stored bytes; the persisted surface says
// otherwise, and the bytes win — in the shape verifier and again, independently, in the promote gate.
//
// The mutation is not invented: it restores what the tool ledger actually held before E21 T7 landed
// toolbroker.Tool.Unretained, which is why this row is the one to read first.
func TestAStoredSearchResultIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := memoryCaseOf(t, m)
	surface, ok := proof["persisted_surface"].(map[string]any)
	if !ok {
		t.Fatalf("the honest proof carries no persisted surface to tamper with: %v", proof["persisted_surface"])
	}
	ledger, _ := surface["tool_calls"].(string)
	surface["tool_calls"] = ledger + ` {"count": 1, "results": [{"said_by": "` + uat.ToolsMemorySpeaker +
		`", "untrusted_text": "` + uat.ToolsMemoryQuotedText + `"}]}`
	// The declared count is left at ZERO on purpose: this is the fabrication, not an oversight.
	if got := proof["stored_search_bytes"]; got != float64(0) {
		t.Fatalf("the honest proof declares %v stored search bytes; this negative needs it at 0", got)
	}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
		t.Fatalf("a run that STORED a search result verified clean: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "in what the run PERSISTED") {
		t.Fatalf("the promote gate did not re-derive the stored bytes from the persisted surface: %v", refusals)
	}
	if !refusalsMention(refusals, "You must not store or copy") {
		t.Fatalf("the refusal does not quote the term it enforces, so a reader cannot tell WHY it fired: %v", refusals)
	}
}

// TestASearchThatFoundNothingIsRefused: a stored-zero means nothing if the search returned nothing worth
// storing. That is the vacuous form of the crown claim and it is refused — the sweep must be shown to FIND
// what it is meant to find before its zero is worth anything.
func TestASearchThatFoundNothingIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := memoryCaseOf(t, m)
	proof["search_reached_the_model"] = map[string]any{"content": "{}", "tool_call_id": "tc_<id>"}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
		t.Fatalf("a proof whose search results never reached the model verified clean: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "is VACUOUS") {
		t.Fatalf("the promote gate did not refuse the vacuous sweep for being vacuous: %v", refusals)
	}
}

// TestAMentionTheModelChoseIsRefused is the second crown negative. The renderer holds exactly ONE identity, so
// a second `<@U…>` on the wire means the model chose who to summon — E20 T1's broadcast hole in its most
// targeted form.
//
// The mutation edits the ANSWER BYTES rather than the counter, which is the whole point: the count is
// re-derived from them, so a proof cannot declare its way out of a token it carries.
func TestAMentionTheModelChoseIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := memoryCaseOf(t, m)
	body, ok := proof["answer_blocks"].(map[string]any)
	if !ok {
		t.Fatalf("the honest proof carries no answer body to tamper with: %v", proof["answer_blocks"])
	}
	blocks, ok := body["blocks"].([]any)
	if !ok || len(blocks) == 0 {
		t.Fatalf("the honest answer carries no blocks: %v", body["blocks"])
	}
	body["blocks"] = append(blocks, map[string]any{
		"type": "section",
		"text": map[string]any{"type": "mrkdwn", "text": "<@U0SOMEONEELSE> take a look"},
	})
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
		t.Fatalf("an answer summoning somebody the model chose verified clean: %v", findings)
	}
	refusals := uat.PromoteGateFor(raw, "rc")
	if !refusalsMention(refusals, "is not the delivery row's frozen requester id") {
		t.Fatalf("the promote gate did not re-derive the mention from the answer's bytes: %v", refusals)
	}
}

// TestAnAnswerThatMentionsNobodyIsRefused is the mention sweep's own vacuity guard: "every token is ours" over
// a message carrying no token proves nothing.
func TestAnAnswerThatMentionsNobodyIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := memoryCaseOf(t, m)
	proof["answer_blocks"] = map[string]any{"channel": "C0TLM", "text": "no mention here",
		"blocks": []any{map[string]any{"type": "markdown", "text": "no mention here"}}}
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
		t.Fatalf("a proof whose sweep never demonstrated it could FIND a mention verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "carry NO mention at all") {
		t.Fatalf("the promote gate did not refuse the vacuous mention sweep: %v", uat.PromoteGateFor(raw, "rc"))
	}
}

// TestAForgedButtonInTheAnswerIsRefused: E20's crown claim rides along in E21's proof, over the SAME bytes
// the mention sweep runs on. A richer render is not a weaker defence, and this is the mechanism rather than
// the sentence.
func TestAForgedButtonInTheAnswerIsRefused(t *testing.T) {
	m := committed(t)
	_, proof := memoryCaseOf(t, m)
	body := proof["answer_blocks"].(map[string]any)
	body["blocks"] = append(body["blocks"].([]any), map[string]any{
		"type": "actions",
		"elements": []any{map[string]any{
			"type": "button", "action_id": "palai_approve", "value": "deadbeef",
			"text": map[string]any{"type": "plain_text", "text": "Approve"},
		}},
	})
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
		t.Fatalf("an answer carrying a forged button verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
		t.Fatal("an answer carrying a forged approval button passed the promote gate")
	}
}

// TestARealPeerClaimIsRefused: this bundle is STRUCTURALLY incapable of claiming a real workspace or a real
// MCP server, and that is the point rather than a limitation.
func TestARealPeerClaimIsRefused(t *testing.T) {
	for _, peer := range []string{"real", "real-workspace", "slack.com", "mcp.slack.com", ""} {
		m := committed(t)
		_, proof := memoryCaseOf(t, m)
		proof["peer"] = peer
		raw := marshal(t, m)
		if findings := uat.VerifyManifest(raw, nil); len(findings) == 0 {
			t.Errorf("a bundle claiming peer %q verified clean — a real receipt is §6 leg 1/4 and no code here can produce one", peer)
		}
		if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
			t.Errorf("a bundle claiming peer %q passed the promote gate", peer)
		}
	}
}

// TestAToolsMemoryCounterThatWasNeverEarnedIsRefused sweeps the counters one at a time. Each row is a claim
// the journey would have had to make FALSELY, and each must be caught on its own — a matrix that only fires
// when several things are wrong at once cannot tell a reader which claim failed.
func TestAToolsMemoryCounterThatWasNeverEarnedIsRefused(t *testing.T) {
	for _, tc := range []struct {
		name  string
		mutga func(proof map[string]any)
	}{
		{"a fold that dropped NOTHING, so no budget was ever reached", func(p map[string]any) {
			p["folded_turns"] = float64(0)
		}},
		{"a fold that dropped EVERYTHING — a truncation, not a window", func(p map[string]any) {
			p["folded_turns"] = p["history_turns"]
		}},
		{"two fold digests that DISAGREE, so the fold is not deterministic", func(p map[string]any) {
			p["fold_digests"] = []any{uat.ToolsMemoryFoldDigest, "sha256:" + strings.Repeat("b", 64)}
		}},
		{"one fold digest, so nothing was folded twice", func(p map[string]any) {
			p["fold_digests"] = []any{uat.ToolsMemoryFoldDigest}
		}},
		{"no tool advertised at all", func(p map[string]any) { p["tools_advertised"] = float64(0) }},
		{"a tool surface nothing was ever DISPATCHED through", func(p map[string]any) {
			p["tools_called_through_real_orchestrator"] = float64(0)
		}},
		{"more calls than tools, a surface the run did not have", func(p map[string]any) {
			p["tools_called_through_real_orchestrator"] = float64(9)
		}},
		{"an external result that DID gain authority", func(p map[string]any) {
			p["external_tool_authority_gained"] = float64(1)
		}},
		{"no external result, so the zero-authority claim is vacuous", func(p map[string]any) {
			p["external_tool_results"] = float64(0)
		}},
		{"no search at all, so the stored-zero is vacuous", func(p map[string]any) {
			p["search_results_returned"] = float64(0)
		}},
		{"a declared stored-byte count above zero", func(p map[string]any) {
			p["stored_search_bytes"] = float64(12)
		}},
		{"no needles, so the M5 sweep could never fire", func(p map[string]any) {
			p["search_result_needles"] = []any{}
		}},
		{"a requester id the answer's mention does not match", func(p map[string]any) {
			p["requester_user_id"] = "U0SOMEONEELSE"
		}},
		{"a mention count the answer's bytes do not support", func(p map[string]any) {
			p["mentions_minted"] = float64(3)
		}},
		{"a mention admitted as minted OUTSIDE our renderer", func(p map[string]any) {
			p["mentions_outside_our_renderer"] = float64(1)
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
			_, proof := memoryCaseOf(t, m)
			tc.mutga(proof)
			raw := marshal(t, m)
			if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_proof is incomplete") {
				t.Errorf("%s verified clean: %v", tc.name, findings)
			}
			if refusals := uat.PromoteGateFor(raw, "rc"); len(refusals) == 0 {
				t.Errorf("%s passed the promote gate", tc.name)
			}
		})
	}
}

// TestTwoToolsMemoryClaimsAreRefused: the promote gate judges the FIRST proof while the verifier checks all of
// them, so a second fabricated anchor could ride behind an honest one. Refuse instead of picking.
func TestTwoToolsMemoryClaimsAreRefused(t *testing.T) {
	m := committed(t)
	kase, _ := memoryCaseOf(t, m)
	clone := map[string]any{}
	for k, v := range kase {
		clone[k] = v
	}
	clone["id"] = memoryAnchorCaseID + "-SECOND"
	m["cases"] = append(m["cases"].([]any), clone)
	raw := marshal(t, m)

	if findings := uat.VerifyManifest(raw, nil); !findingsMention(findings, "tools_memory_claims") {
		t.Fatalf("two tools-memory anchors in one manifest verified clean: %v", findings)
	}
	if refusals := uat.PromoteGateFor(raw, "rc"); !refusalsMention(refusals, "tools_memory_claims in one manifest") {
		t.Fatalf("two anchors passed the promote gate: %v", refusals)
	}
}

// TestATierThatADVANCEDIsRefused proves the composed clauses are live in THIS family: E21 inherits "no tier
// advances against the committed E17 baseline" through E20 and E19 rather than restating it, so a bundle that
// recomputed `slack` to stable is refused here without this file owning a tier rule of its own.
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
		if snapshot, _ := proof["snapshot"].(map[string]any); snapshot != nil {
			snapshot["slack"] = "stable"
		}
	}
	if !moved {
		t.Fatal("the bundle declares no slack tier to move — the negative has nothing to prove")
	}
	refusals := uat.PromoteGateFor(marshal(t, m), "rc")
	if len(refusals) == 0 {
		t.Fatal("a bundle declaring `slack` STABLE passed the tools-memory promote gate — the composed no-tier-advance clause is not running")
	}
	t.Logf("a fabricated stable tier was refused for %d reason(s); first: %s", len(refusals), refusals[0])
}

// TestNoMCPCapabilityWasOpened is the plan's explicit §3.6 D9 requirement, asserted mechanically rather than
// promised in prose. Opening an `mcp` key would move CapabilityClaimsDigest and redden every case checksum in
// nineteen committed bundles — and it would advertise as a discovery surface something that is an admin
// connection. The counter-argument ("tools work now, so say so") is real; the answer is that `/v1/capabilities`
// is not where that belongs.
func TestNoMCPCapabilityWasOpened(t *testing.T) {
	for _, capability := range uat.CapabilityTierOrder {
		if capability == "mcp" {
			t.Fatal("an `mcp` capability was opened — §3.6 D9: that moves CapabilityClaimsDigest and reddens " +
				"every case checksum in every committed bundle, to advertise an admin connection as a " +
				"discovery surface")
		}
	}
	if _, governed := uat.CapabilityClaims["mcp"]; governed {
		t.Fatal("uat.CapabilityClaims governs an `mcp` capability that CapabilityTierOrder does not list — the two must not disagree")
	}
	// And the two tiers this epic DID touch the subject of stay where they were, for reasons that are on the
	// record: `slack` on §6 leg 1, `knowledge-vector` on a missing vector store AND — new in E21 — on M5,
	// which forbids storing the data an index would be built from.
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
		for capability, want := range map[string]string{"slack": "preview", "knowledge-vector": "disabled"} {
			if got := c.Proof.Snapshot[capability]; got != want {
				t.Errorf("this release closes `%s` at %q, want %q — E21 grew the surface, which is the least earned moment for a tier", capability, got, want)
			}
		}
		return
	}
	t.Fatalf("the bundle carries no %s tier snapshot to check", tierAnchorCaseID)
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
		{"the counter-argument is NAMED (tools work and the workspace is searchable)", "tools work now and the workspace is searchable"},
		{"reason 1 — §6 leg 1 is still open and Peer is structurally fake", "NO CAPTURED RECEIPT"},
		{"reason 2 — the crown feature rests on Slack's NEWEST API and the youngest fake in the tree", "YOUNGEST fake in this repository"},
		{"reason 3 — E21 grows the surface, which is the least earned moment for a tier", "GROWS THE SURFACE AGAIN"},
		{"the defect this gate found, so a reader meets it in the evidence", "sat there verbatim"},
		{"the term that defect broke, quoted", "You must not store or copy"},
		{"the MCP server's rejection and its cause", "no authorization-code flow"},
		{"no mcp capability was opened, with the cost of opening one", "nineteen committed bundles"},
	} {
		if !strings.Contains(body, required.needle) {
			t.Errorf("the bundle does not carry %s (looked for %q) — the plan requires the tier counter-argument be stated and refused ON THE RECORD, not in a gate's comments", required.what, required.needle)
		}
	}
}
