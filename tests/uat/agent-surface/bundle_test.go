// Package agentsurface holds the E20 EXIT gate (plan §T5): the slack-agent-surface-0.1.0 evidence bundle,
// the forgery-derivation refusal matrix, the agent-surface promote gate, the four UAT cases this epic
// OPENED, and the credential-gated live inventory collapsed to one command. Everything here is Docker-free
// pure logic, so it rides `make verify`; the Docker-bound journey is driven from journey_test.go through
// scripts/uat/agent-surface.
//
// WHAT THIS BUNDLE CLAIMS, AND THE DISTINCTION IS IN ITS NAME: `slack-agent-surface`, never
// `slack-agent-verified`. It certifies that a working Slack integration became an AGENT SURFACE — a panel, a
// working status, a stream that fills in as the run works, and rich Block Kit at the end — that every one of
// those entrances funnels through the ONE admission bridge, and that the model is structurally unable to
// mint anything a human can press.
//
// WHAT IT DOES NOT CLAIM: that any of it worked in a real Slack workspace. A real workspace IS connected and
// a real run DID complete on 2026-07-26 — and that run is refused as evidence here for three reasons written
// out in TestTheTierDecisionIsOnTheRecord. `slack` closes PREVIEW.
package agentsurface

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/palgroup/palai/tests/uat"
)

// repoRoot resolves the repository root from THIS source file, so the gate finds the committed corpus no
// matter the process working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve this file's path")
	}
	return filepath.Join(filepath.Dir(self), "..", "..", "..")
}

func bundleDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.AgentSurfaceBundle)
}

// baselineManifest is the committed E19 EXIT bundle. This gate DERIVES its inherited case set from it rather
// than retyping one: the surface E20 grew is the surface E19 wired, so this release's cases ARE that
// release's cases plus the four ids E20 opened plus the surface anchor. Retyping them would let the two
// drift, and a drifted case set is how a release quietly drops the red case that caps a tier.
const baselineManifest = uat.WiringBundle

// surfaceAnchorCaseID is the release-level entry the agent-surface proof hangs off. Like E19-WIRING and
// E17-TIER it is NOT a UAT case (it has no tests/uat/cases directory): "the model cannot mint a button" is
// not a behaviour one case asserts, it is an observation over a whole journey's outbound traffic.
const surfaceAnchorCaseID = "E20-SURFACE"

// tierAnchorCaseID is the E17 tier table's entry, carried forward: a bundle carrying E17 area claims must
// carry exactly one capability tier table, and this release is judged on the tier it did NOT move.
const tierAnchorCaseID = "E17-TIER"

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the wiring / extensions / self-host gates keep. A drift between this copy
// and the verifier's shows up immediately as a bundle whose checksums do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the wiring/extensions precedent: this journey is
// COMPONENT-tier (real PostgreSQL, no engine container), so there is no real engine image to name and the
// shape verifier's required field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e20", 21) + "e"

// newCaseIDs is the four ids E20 opened, read from the CANONICAL table rather than retyped so this bundle
// and the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.AgentSurfaceCaseIDs }

// buildAgentSurfaceManifest assembles the bundle. Every inherited case comes from the committed E19
// baseline; every proof body comes from a canonical uat table. Nothing is typed twice.
func buildAgentSurfaceManifest(t *testing.T) []byte {
	t.Helper()

	raw, err := os.ReadFile(filepath.Join(repoRoot(t), "evidence", "releases", baselineManifest, "manifest.json"))
	if err != nil {
		t.Fatalf("read the %s baseline (this release's inherited case set is derived from it, never retyped): %v", baselineManifest, err)
	}
	var base struct {
		Cases []map[string]any `json:"cases"`
	}
	if err := json.Unmarshal(raw, &base); err != nil {
		t.Fatalf("decode the baseline: %v", err)
	}

	anchor := uat.AgentSurfaceContractsDigest()
	cases := make([]map[string]any, 0, len(base.Cases)+len(newCaseIDs())+1)
	outcomes := make(map[string]string, len(base.Cases)+len(newCaseIDs())+1)

	for _, bc := range base.Cases {
		id, _ := bc["id"].(string)
		if id == "" {
			t.Fatalf("the baseline carries a case with no id: %v", bc)
		}
		c := map[string]any{}
		for k, v := range bc {
			c[k] = v
		}
		runID := "run_e20_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E20 AGENT-SURFACE RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that the surface it exercises GREW (a panel, a working status, a run that "+
				"streams, and rich Block Kit), and every new entrance funnels through the SAME admission this "+
				"case already covers. The counterparty is still a documented FAKE, so the case's §6 operator leg "+
				"is untouched and its capability's tier does not move. E20 makes leg 1 BIGGER and CHEAPER, not closed.")
		cases = append(cases, c)
		outcomes[id] = strings.TrimSpace(toString(c["status"]))
	}

	// ---- the four ids E20 OPENED ----------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e20_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": "component-real",
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e20_deterministic",
			"mtls_enroll":         "component-tier: no runner enrollment",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E20 and certified HERE rather than in extensions-0.1.0, which is a measured decision: " +
					"tests/uat/extensions' catalog IS that shipped bundle's case list and uat.CapabilityClaims feeds a " +
					"digest folded into its every checksum, so registering these four there would have forced the " +
					"regeneration of a committed HISTORICAL release. A case belongs to the bundle that certifies it",
				"NOT in uat.CapabilityClaims[\"slack\"] and therefore NOT an input to the `slack` tier recompute — " +
					"which changes nothing about the outcome: `slack` is capped at preview by CapabilityOperatorLegs " +
					"(§6 leg 1) whatever its claims do, and four MORE green claims could never have raised it",
				"HONEST CEILING: every counterparty is the FAKE Slack peer built from the published references. §6 " +
					"leg 1 — a REAL Slack workspace external receipt — is operator work and is NEVER claimed here",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
		outcomes[id] = "PASS"
	}

	// ---- the surface anchor: the entrances, the counters, and the forgery re-derivation ----------------
	surfaceCase := map[string]any{
		"id": surfaceAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_e20_agent_surface_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e20_deterministic",
		"mtls_enroll":         "component-tier: no runner enrollment",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"the model cannot mint an actionable element\" is not a behaviour one case asserts, it is an observation over a whole journey's outbound traffic — the E19-WIRING / E17-TIER precedent",
			"every counter here was MEASURED by apps/control-plane/internal/store TestAgentSurfaceJourney against REAL PostgreSQL and the shipped routes, in the SAME process that served them: three entrances (an HTTP callback, a Socket Mode envelope and a panel DM), five deliveries of three source events, three runs, three visible messages",
			"the source event ids are read back out of the idempotency_records the SHARED Admit wrote, not out of the journey's own bookkeeping — only that admission writes those rows, so the count IS the evidence that the panel opened no second admission path",
			"the CROWN claim is RE-DERIVED, never believed: the closing message's actual blocks travel in this proof and uat.SweepActionableElements recomputes the actionable-element count from those bytes, in Complete() AND independently in AgentSurfacePromoteGate. A bundle declaring zero over a closing message containing a button is refused by construction",
			"the sweep is shown to DISCRIMINATE before it is trusted: pointed at interactions.go's ApprovalMessage it finds seven actionable nodes, and pointed at the renderer's output for a model answer that FORGES `{\"type\":\"actions\",…,\"action_id\":\"palai_approve\"}` it finds zero — the forgery arrives as characters in markdown_text, where a human reads it and nobody can press it",
			"NO TIER ADVANCES: uat.AgentSurfacePromoteGate composes uat.WiringPromoteGate, which REFUSES any capability sitting higher than the committed " + baselineManifest + " lineage's " + "extensions-0.1.0 baseline. A wider surface is the LEAST earned moment for a tier — see the three refusal reasons in the tier_decision assertions below",
			"TIER DECISION, ARGUED AND REFUSED ON THE RECORD (1/3): a real workspace IS connected and a real run DID complete on 2026-07-26 (gpt-4o-mini-2024-07-18, 316 in / 153 out), but that run left NO CAPTURED RECEIPT and SlackAgentSurfaceProof.Peer is structurally the literal \"fake\". Moving the tier first requires DESIGNING what a real receipt IS — a transcript from which a verifier can re-derive workspace id + event id + run id — and that design is the content of §6 leg 1 itself",
			"TIER DECISION (2/3): that run demonstrated ONE claim (a mention births a run), not the eight of SLK-001..008 — and E20 adds four more, so the fraction of the surface it covers went DOWN, not up",
			"TIER DECISION (3/3): E20 GROWS the surface. agent_view, streaming and task cards are Slack's newest surfaces (2026-07, 2025-10 and 2026-02 respectively) and the YOUNGEST fakes in this repository are built against them. Raising a tier in the epic that grows the surface most is exactly what this gate exists to prevent. E20's contribution to leg 1 is to make it BIGGER and CHEAPER — the leg's text now covers the panel, the stream and the render, and `make uat-agent-surface-live` reduces its execution to ONE command",
			"FIVE REQUIREMENTS ARE NOT IN THE CONTRACT LEDGER AND THAT IS THE HONEST ANSWER: plan §3.5 S16 names five things the vendor documentation does not state (what an unstopped stream does, whether chat.update works on a streaming message, whether a Socket-Mode-only app may call the streaming Web API, what the video block's Events-API exception opens, and whether interactive elements render in the panel). None entered the code as an assumption; each is a row in docs/operations/known-gaps-1.0.md and each is measured by §6 leg 1/2",
		},
		"agent_surface_claim": "one-admission-bridge-behind-a-wider-surface-and-a-model-that-cannot-mint-a-button",
		"agent_surface_proof": canonicalAgentSurfaceProof(),
	}
	surfaceCase["checksum"] = hashParts(surfaceAnchorCaseID, surfaceCase["run_id"].(string), anchor)
	cases = append(cases, surfaceCase)
	outcomes[surfaceAnchorCaseID] = "PASS"

	// ---- the tier table, RECOMPUTED from this bundle's own outcomes ------------------------------------
	recomputed := uat.RecomputeCapabilityTiers(outcomes)
	for _, c := range cases {
		if c["id"] != tierAnchorCaseID {
			continue
		}
		declarations := make([]uat.CapabilityTierDeclaration, 0, len(uat.CapabilityTierOrder))
		snapshot := make(map[string]string, len(uat.CapabilityTierOrder))
		for _, capability := range uat.CapabilityTierOrder {
			declarations = append(declarations, uat.CapabilityTierDeclaration{
				Capability: capability, DeclaredTier: recomputed[capability],
				ClaimCaseIDs: uat.CapabilityClaims[capability],
			})
			snapshot[capability] = recomputed[capability]
		}
		c["capability_tier_proof"] = uat.CapabilityTierProof{
			Capabilities: declarations, Snapshot: snapshot,
			SnapshotSource: "carried forward from " + baselineManifest + ", where it was read over real HTTP from a FULLY MOUNTED router (apps/control-plane/internal/store TestWiringJourney). E20 mounts no new route and moves no tier, so the snapshot it would take is the snapshot that release already earned — and the promote gate re-derives the comparison from committed bytes rather than trusting this sentence",
			ClaimsDigest:   uat.CapabilityClaimsDigest(),
		}
	}

	manifest := map[string]any{
		"release":     uat.AgentSurfaceBundle,
		"git_sha":     "b588a0a",
		"api_version": "v1",
		"migration":   "000040_capability_workers",
		"captured_at": "2026-07-27T00:00:00Z",
		"maturity":    "rc",
		"cases":       cases,
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetIndent("", "  ")
	if err := enc.Encode(manifest); err != nil {
		t.Fatalf("encode manifest: %v", err)
	}
	return buf.Bytes()
}

// canonicalAgentSurfaceProof is the proof body the bundle carries. Every counter is what
// TestAgentSurfaceJourney measured against real PostgreSQL; the contract ledger and the closing blocks are
// READ FROM CANONICAL SOURCES rather than retyped — the ledger from uat.AgentSurfaceContracts, the blocks
// from the SHIPPED renderer via uat.AgentSurfaceClosingBlocks — so neither can drift into the bundle.
func canonicalAgentSurfaceProof() uat.SlackAgentSurfaceProof {
	return uat.SlackAgentSurfaceProof{
		Peer: uat.AgentSurfacePeer,
		// Three runs, three visible messages: two streams that opened on a journal line (the mention's first
		// model step, and the approval seed's approval.requested.v1) and one plain post for the run that
		// produced no journal line before its terminal.
		Runs:                       3,
		VisibleMessages:            3,
		AdmissionEntrances:         uat.AgentSurfaceEntrances,
		AdmissionRoute:             uat.AgentSurfaceAdmissionRoute,
		AdmittedThroughSharedAdmit: 3,
		SourceEventIDs:             []string{"EvSurfacePanelDM", "EvSurfaceChannel", "EvApprovalSeed"},
		// Five deliveries of three source events: the panel DM twice (HTTP then Socket Mode), the channel
		// mention twice (Socket Mode then an HTTP redelivery), the approval seed once.
		Deliveries:                               5,
		ContextEntitiesDescribed:                 1,
		ContextEntitiesGrantedAuthority:          0,
		ContextChannelReads:                      0,
		ApprovalBuilderMints:                     7,
		ActionableElementsOutsideApprovalBuilder: 0,
		ClosingBlocks:                            uat.AgentSurfaceClosingBlocks(),
		Contracts:                                uat.AgentSurfaceContracts,
		ContractsDigest:                          uat.AgentSurfaceContractsDigest(),
	}
}

func toStrings(t *testing.T, id string, v any) []string {
	t.Helper()
	list, ok := v.([]any)
	if !ok {
		t.Fatalf("%s: db_assertions is not a list: %T", id, v)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			t.Fatalf("%s: a db_assertion is not a string: %T", id, e)
		}
		out = append(out, s)
	}
	return out
}

func toString(v any) string {
	s, _ := v.(string)
	return s
}

// TestCommittedAgentSurfaceBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a renderer change (the closing
// blocks come from the SHIPPED renderer), a case-set change or a tier change cannot leave a stale bundle
// verifying green.
// Regenerate with: PALAI_WRITE_AGENT_SURFACE_BUNDLE=1 go test ./tests/uat/agent-surface/
func TestCommittedAgentSurfaceBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildAgentSurfaceManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_AGENT_SURFACE_BUNDLE") == "1" {
		if err := os.MkdirAll(bundleDir(t), 0o755); err != nil {
			t.Fatalf("create release dir: %v", err)
		}
		if err := os.WriteFile(path, want, 0o644); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
		t.Logf("wrote %s", path)
		return
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_AGENT_SURFACE_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_AGENT_SURFACE_BUNDLE=1", uat.AgentSurfaceBundle)
	}
}

// TestAgentSurfaceReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=slack-agent-surface-0.1.0` gate,
// in-process.
func TestAgentSurfaceReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the agent-surface release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the agent-surface bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the agent-surface bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.AgentSurfaceBundle, summary.String())
}

// TestTheFourNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED four
// ids must carry all four, or the ids exist in the tree with no bundle certifying them — which is precisely
// the state T3 and T4 refused to leave behind.
func TestTheFourNewCasesAreInTheBundle(t *testing.T) {
	var m struct {
		Cases []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"cases"`
	}
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode the bundle: %v", err)
	}
	carried := map[string]string{}
	for _, c := range m.Cases {
		carried[c.ID] = c.Status
	}
	for _, id := range uat.AgentSurfaceCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	if _, ok := carried[surfaceAnchorCaseID]; !ok {
		t.Errorf("the bundle carries no %s anchor — without it the forgery derivation never runs", surfaceAnchorCaseID)
	}
}

// TestAgentSurfaceBundleNeverClaimsARealWorkspace is the honest-ceiling guard, and it is deliberately about
// the TEXT rather than the proof struct: Complete() already refuses a Peer other than "fake", so what
// remains to catch is prose that overclaims around a mechanically-honest proof — the way an evidence bundle
// actually misleads a reader.
//
// The scan runs PER SENTENCE and a sentence carrying a negation marker is the honest form, because this
// bundle's own tier-decision paragraphs must NAME the real run in order to refuse it. A blunt whole-file
// substring scan cannot tell "a real workspace is connected" from "a real workspace receipt is NEVER claimed
// here", and would fire on exactly the text it exists to protect.
func TestAgentSurfaceBundleNeverClaimsARealWorkspace(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	negationMarkers := []string{"§6 leg", "never", "not ", "no ", "nothing", "refus", "untouched", "unmet", "fake", "preview"}
	for _, sentence := range strings.Split(string(raw), ". ") {
		lower := strings.ToLower(sentence)
		for _, forbidden := range []string{"real slack workspace", "real workspace receipt", "in production", "verified in slack"} {
			if !strings.Contains(lower, forbidden) {
				continue
			}
			negated := false
			for _, marker := range negationMarkers {
				if strings.Contains(lower, marker) {
					negated = true
					break
				}
			}
			if !negated {
				t.Errorf("the bundle names %q in a sentence that does NOT negate it — this release contacted no workspace and may not imply one:\n  %s", forbidden, strings.TrimSpace(sentence))
			}
		}
	}
	for _, required := range []string{"§6 leg 1", "\"fake\"", "PREVIEW", "NO CAPTURED RECEIPT"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("the bundle never mentions %q — the ceiling has to be legible in the manifest itself, not only in this gate's comments", required)
		}
	}
}

// TestAgentSurfaceBundleCarriesEveryDivergenceRow is the §3.5 completeness check. The plan's crown output is
// the divergence table, so an epic that implemented a surface while silently dropping the row that named its
// gap would be exactly the regression this family exists to prevent.
func TestAgentSurfaceBundleCarriesEveryDivergenceRow(t *testing.T) {
	seen := map[string]bool{}
	for _, req := range uat.AgentSurfaceContracts {
		seen[req.Divergence] = true
		if req.SourceURL == "" || req.Requirement == "" {
			t.Errorf("divergence %s carries no source URL or no requirement text — a requirement nobody can audit is not grounding", req.Divergence)
		}
	}
	// S1..S15 and S17..S20 are the plan §3.5 rows E20's surface implements. S16 is checked separately and
	// must be ABSENT.
	for _, row := range []string{"S1", "S2", "S3", "S4", "S5", "S6", "S7", "S8", "S9", "S10",
		"S11", "S12", "S13", "S14", "S15", "S17", "S18", "S19", "S20"} {
		if !seen[row] {
			t.Errorf("§3.5 row %s is not in the contract ledger — the divergence table is the plan's crown output and a dropped row is a silently reintroduced gap", row)
		}
	}
}

// TestAgentSurfaceLedgerRefusesToCarryTheUnconfirmedRow pins the one row that must NOT be there. S16 is the
// five requirements the vendor documentation does not state; putting them in a ledger of "published
// requirements this surface implements" would dress five unknowns as five citations. Their home is
// docs/operations/known-gaps-1.0.md, and TestTheUnconfirmedRequirementsAreInKnownGaps checks they arrived.
func TestAgentSurfaceLedgerRefusesToCarryTheUnconfirmedRow(t *testing.T) {
	for _, req := range uat.AgentSurfaceContracts {
		if req.Divergence == "S16" {
			t.Errorf("the contract ledger carries S16 (%q) — that row is FIVE THINGS THE DOCUMENTATION DOES NOT SAY, and a ledger entry would give them a source URL they do not have", req.Requirement)
		}
	}
}
