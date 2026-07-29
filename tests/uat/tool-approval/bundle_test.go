// Package toolapproval holds the E23 EXIT gate (plan §T7): the tool-approval-0.1.0 evidence bundle, the
// refusal matrix, the tool-approval promote gate, the five UAT cases this epic OPENED, and the operator
// entry point collapsed to one command. Everything here is Docker-free pure logic, so it rides `make
// verify`; the Docker-bound journey is driven from journey_test.go through scripts/uat/tool-approval.
//
// WHAT THIS BUNDLE CLAIMS, and it is deliberately narrower than "nothing dangerous can happen now": an
// approval line in which every side-effecting tool call CAN pass through a human's button; that human sees
// the CALL ITSELF and not a description — identity from the server, label from the operator, arguments
// verbatim, and the button bound to their hash; the run parks while waiting and does not wait forever; the
// approver is a project policy and not a Slack list; and no byte from outside — not a ticket body, not an
// MCP server's description — can write its own approval.
//
// THE DEFINING DECISION OF THIS EPIC IS A GENERALISATION: approval is not a publication feature, it is a
// TOOL-CALL feature. And the answer to its central design question is the vendor's rather than ours —
// verbatim "Show tool inputs to the user before calling the server" and "clients MUST consider tool
// annotations to be untrusted unless they come from trusted servers" (MCP 2025-11-25, fetched 2026-07-29).
//
// WHAT IT DOES NOT CLAIM: that any of it ran against a real Slack workspace, a real Atlassian tenant or a
// real GitHub App, and — the epic's own D1 — that the GENERIC half has a production decision surface at all.
// uat.ToolApprovalPeer is structurally the literal "fake". NO TIER ADVANCES.
package toolapproval

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
	return filepath.Join(repoRoot(t), "evidence", "releases", uat.ToolApprovalBundle)
}

// baselineManifest is the committed E22 EXIT bundle. This gate DERIVES its inherited case set from it rather
// than retyping one: the approval line E23 built gates the code-and-publish line E22 built, so this
// release's cases ARE that release's cases plus the five ids E23 opened plus the approval anchor. Retyping
// them would let the two drift, and a drifted case set is how a release quietly drops the red case that
// caps a tier.
const baselineManifest = uat.CodeAndShipBundle

// approvalAnchorCaseID is the release-level entry the tool-approval proof hangs off. Like E22-CODE-AND-SHIP,
// E21-TOOLS-MEMORY, E20-SURFACE, E19-WIRING and E17-TIER it is NOT a UAT case (it has no tests/uat/cases
// directory): "nothing side-effecting ran without a human" is not a behaviour one case asserts, it is an
// observation over everything a whole journey did and did not do.
const approvalAnchorCaseID = "E23-TOOL-APPROVAL"

// The anchors carried forward from the releases underneath. A bundle carrying E22 cases must carry the
// code-and-ship anchor, one carrying E21 cases the tools-memory anchor, and so down — so all four ride along
// unchanged, and this release is judged on the tier it did NOT move.
const (
	shipAnchorCaseID    = "E22-CODE-AND-SHIP"
	memoryAnchorCaseID  = "E21-TOOLS-MEMORY"
	surfaceAnchorCaseID = "E20-SURFACE"
	tierAnchorCaseID    = "E17-TIER"
)

// hashParts reproduces the tests/uat construction (sha256 of each part followed by a NUL, hex-encoded,
// sha256:-prefixed) so this generator and the verifier derive the same re-derivable values.
// ponytail: the same 6-line copy the code-and-ship / tools-memory / agent-surface / wiring / extensions
// gates keep. A drift between this copy and the verifier's shows up immediately as a bundle whose checksums
// do not recompute.
func hashParts(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// syntheticImageDigest is an obviously-unservable digest, the code-and-ship / tools-memory precedent: this
// journey is COMPONENT-tier (real PostgreSQL, no engine container), so there is no real engine image to name
// and the shape verifier's required field carries a value no registry could ever serve.
var syntheticImageDigest = "sha256:" + strings.Repeat("e23", 21) + "e"

// newCaseIDs is the five ids E23 opened, read from the CANONICAL table rather than retyped so this bundle
// and the orphan guards can never disagree about which cases exist.
func newCaseIDs() []string { return uat.ToolApprovalCaseIDs }

// buildToolApprovalManifest assembles the bundle. Every inherited case comes from the committed E22
// baseline; every proof body comes from a canonical uat table or from a SHIPPED function. Nothing is typed
// twice.
func buildToolApprovalManifest(t *testing.T) []byte {
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

	anchor := uat.ToolApprovalContractsDigest()
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
		runID := "run_e23_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c["run_id"] = runID
		c["checksum"] = hashParts(id, runID, anchor)
		c["db_assertions"] = append(append([]string{}, toStrings(t, id, bc["db_assertions"])...),
			"E23 TOOL-APPROVAL RELEASE: this case's LOCAL seam is unchanged from "+baselineManifest+
				" — what changed is that a tool call reaching it can now WAIT FOR A HUMAN. The gate is "+
				"declared at REGISTRATION on the tool (never inferred from a server's own annotations, which "+
				"the MCP specification says a client MUST treat as untrusted), the run PARKS rather than "+
				"answering, an approval nobody presses EXPIRES and releases the run, and the approver is a "+
				"PROJECT POLICY checked in the one function both surfaces pass through. The counterparty is "+
				"still a documented FAKE, so this case's §6 operator leg is untouched and its capability's "+
				"tier does not move. E23 makes §6 leg 1 BIGGER again — it now covers the approval message, "+
				"the modal, an unauthorized click and a merge — and it OPENS §6 leg 6, an approval expiring "+
				"in REAL time, which is NOT closed either.")
		cases = append(cases, c)
		outcomes[id] = strings.TrimSpace(toString(c["status"]))
	}

	// ---- the five ids E23 OPENED ----------------------------------------------------------------------
	for _, id := range newCaseIDs() {
		runID := "run_e23_" + strings.ToLower(strings.ReplaceAll(id, "-", "_"))
		c := map[string]any{
			"id": id, "status": "PASS", "proof_class": "component-real",
			"run_id":              runID,
			"image_digest":        syntheticImageDigest,
			"provider_request_id": "prov_e23_deterministic",
			"mtls_enroll":         "component-tier: no runner enrollment",
			"terminal":            map[string]any{"type": "response.completed", "count": 1},
			"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
			"db_assertions": []string{
				caseAssertion(t, id),
				"OPENED BY E23 UNDER THE `HIL-` PREFIX, and that prefix is a GATE decision rather than a naming " +
					"preference: an `SLK-` id would have regenerated either the shipped extensions-0.1.0 bundle or " +
					"the shipped slack-agent-surface-0.1.0 bundle, and an id already inside AgentSurfaceCaseIDs, " +
					"ToolsMemoryCaseIDs or CodeAndShipCaseIDs would have matched an EARLIER family marker in " +
					"PromoteGateFor and dispatched this release to a WEAKER gate that knows nothing about the " +
					"ungoverned-side-effect sweep or the screen-authorship fence. A case belongs to the bundle that " +
					"certifies it, and the prefix is what keeps dispatch unambiguous",
				"AND THE PREFIX HAD A HOLE THIS GATE CLOSED: HIL-004 shipped in T2 while `HIL-` was in NO prefix " +
					"list, so that directory escaped tests/uat/extensions' orphan sweep — the ONLY sweep in the tree " +
					"that walks tests/uat/cases — and nothing resolved its proofs. The prefix is in " +
					"extensionIDPrefixes now, the sweep defers to uat.ToolApprovalCaseIDs, and " +
					"TestTheHILPrefixIsGuardedBySweep fails if either half is removed",
				"NOT in uat.CapabilityClaims and therefore NOT an input to any tier recompute — which changes " +
					"nothing about the outcome: `slack` is capped at preview by CapabilityOperatorLegs (§6 leg 1) " +
					"whatever its claims do, `knowledge-vector` and `apple-build` are `disabled` by construction, " +
					"and NO new capability was opened (adding a member to CapabilityTierOrder would shift " +
					"CapabilityClaimsDigest and redden every case checksum in every committed bundle)",
				"HONEST CEILING: every counterparty is a FAKE built from the published references — a fake Slack, " +
					"a fake Atlassian MCP server, a fake GitHub App publisher and a fake provider. §6 leg 1 (a REAL " +
					"Slack workspace external receipt), §6 leg 5 (a REAL GitHub App merging a REAL pull request) and " +
					"§6 leg 6 (an approval expiring in REAL time) are operator work and are NEVER claimed here",
			},
		}
		c["checksum"] = hashParts(id, runID, anchor)
		cases = append(cases, c)
		outcomes[id] = "PASS"
	}

	// ---- the tool-approval anchor: the gate, the screen, the park and the approver ---------------------
	approvalCase := map[string]any{
		"id": approvalAnchorCaseID, "status": "PASS", "proof_class": "component-real",
		"run_id":              "run_e23_tool_approval_journey",
		"image_digest":        syntheticImageDigest,
		"provider_request_id": "prov_e23_deterministic",
		"mtls_enroll":         "component-tier: no runner enrollment",
		"terminal":            map[string]any{"type": "response.completed", "count": 1},
		"usage":               map[string]any{"input_tokens": 0, "output_tokens": 0, "total_tokens": 0},
		"db_assertions": []string{
			"RELEASE-LEVEL entry, not a UAT case (it has no tests/uat/cases directory): \"nothing side-effecting ran without a human\" is not a behaviour one case asserts, it is an observation over everything a whole journey did and did not do — the E22-CODE-AND-SHIP / E21-TOOLS-MEMORY / E20-SURFACE / E19-WIRING / E17-TIER precedent",
			"THE CROWN CLAIM IS THAT A SIDE-EFFECTING TOOL CALL CANNOT RUN WITHOUT A HUMAN, AND THE GATE IS DECLARED AT REGISTRATION RATHER THAN GUESSED. It is RE-DERIVED from the call ledger rather than declared: every gated call that executed must carry an approve, and the ledger must itself demonstrate all four shapes — an approve that RAN, a deny that did not, an EXPIRY that did not, and an ungated call that ran untouched — because a zero over a corpus nobody ever approved certifies nothing, and a gate that never let anything through is indistinguishable from a broken tool. THE DECLARATION CANNOT COME FROM THE SERVER: the MCP specification says verbatim that clients \"MUST consider tool annotations to be untrusted unless they come from trusted servers\" (fetched 2026-07-29, §3.5 P3), so `destructiveHint` is not a gate — our client does not decode `annotations` at all — and the flag is `tool_revisions.approval_required` (000044 R3) beside the per-tool publish an operator already makes",
			"THE SECOND CROWN CLAIM IS THAT THE HUMAN READS THE CALL AND NOT A DESCRIPTION, and it is this epic's cheapest security test. The approval screen is COMPUTED from three parts whose provenance is fixed by construction — the identity resolved by the lookup that will EXECUTE the call, the one sentence an OPERATOR wrote at registration (or the literal `(no operator label)`), and the ARGUMENTS IN FULL, key-sorted and neutralized — and the proof re-derives, from the screen's own bytes, that the intersection with everything the MCP server and the model wrote is EMPTY. The sweep runs in BOTH directions: the server's `description` and `title` and the model's prose must be findable in what ARRIVED FROM OUTSIDE, or the zero is a statement about text nobody sent. WHEN THE NEXT READER SAYS \"let's also show the tool's description, it helps the user\", THAT LINE IS THE ANSWER: the vendor documents `title` and `description` as the human-readable display fields and, on the same page, says not to trust them — the two fields recommended FOR display are written by the party with an interest in the answer",
			"THE THIRD CROWN CLAIM IS THAT THE RUN PARKS AND DOES NOT WAIT FOREVER, and its second half had NO implementation in this tree at all. `approvals.expires_at` was declared in migration 000013 in 2023 and nothing ever wrote a value into it — harmless while nothing waited, and a run parked forever the moment something did. Now every approval carries a deadline, a reaper cancels the lapsed call, journals `approval.expired.v1` through an EXISTING event type, and WAKES THE PARKED RUN; approve, deny and the reaper all end in ONE wake function, because a gate whose closing and opening are written twice is a gate that one day only closes. The proof re-derives both halves from the run ledger: zero runs went terminal while a human still owed an answer (the E22 defect this epic repairs), and zero runs were left waiting after an expiry",
			"THE FOURTH CROWN CLAIM IS THAT THE APPROVER IS A PROJECT POLICY AND NOT A SLACK LIST, checked in ONE throat. `approvals.allowed_approver` existed since 000013, exactly one line wrote it, NO line ever read it, and on the HTTP surface the concept of an approver did not exist — any tenant-scoped API key carried an approve through. The list is `config_policy.approvers` (no migration: the JSONB column and its write path are shipped), each surface maps its identity onto one canonical principal, and the check sits in ApplyApprovalDecision rather than in each caller. LIST PRESENT ⇒ deny by default; LIST ABSENT ⇒ BIT-UNCHANGED, non-negotiable and asserted. The proof re-derives that an unauthorized principal was refused on BOTH surfaces, because a ledger refusing on only one would be consistent with a guard bolted onto a single caller",
			"THE BUTTON IS BOUND TO THE BYTES: `tool_calls.request_hash` is toolbroker.RequestHash(name, args), so what a human approved is THOSE ARGUMENTS — a call whose arguments moved between the question and the answer has no approval and refuses loudly, and what executes is read back off the approval's own ledger row rather than off the replaying attempt's frame, which closes the quieter half where the frame agrees but a non-deterministic transform substituted something the human never read",
			"THE DESTINATION STILL DOES NOT COME FROM THE MODEL, AND MERGE IS THE STRONGEST CASE OF IT: `palai.publish.merge_pull_request` takes NO INPUT AT ALL. WHICH pull request is the one this run opened, read back from its own published receipt; AT WHICH commit is the approved publication's head_sha; HOW is the binding's merge_method. GitHub documents `sha` as OPTIONAL — 409 \"if sha was provided and pull request head did not match\" (fetched 2026-07-29, §3.5 P16) — and this tree makes it MANDATORY, so a head that moved after the approval takes a 409 and nothing merges. The vendor hands over a race guard for free and this epic does not decline it. The sweep runs over all THREE publish tools' input schemas and matches by substring, because `pr` and `pull_request_number` are the same field asked for differently",
			"THE APPROVAL SCREEN HAS BUTTONS AND ONLY ONE FILE MAY BUILD ONE, which is the honest form of E20's claim rather than a weaker one: the count is non-zero on purpose (a screen with no button is a screen nobody can decide from) and what is re-derived is the MINT SITE. This proof parses two packages — adapters/integrations/slack AND apps/control-plane/internal/extensions — and requires interactions.go to be the only file building an actionable element. That is strictly WIDER than the adapter's own sweep, which reads a single directory (§3.6 D13), and the reason it is wider is this epic: the modal is opened from `extensions/`, so a view constructed there would have left the narrow scan green while the claim it certifies became false",
			"NO TIER ADVANCES: uat.ToolApprovalPromoteGate composes uat.CodeAndShipPromoteGate, which composes tools-memory, agent-surface and wiring, which REFUSES any capability sitting higher than the committed extensions-0.1.0 baseline. See the tier_decision assertions below",
			"TIER DECISION, ARGUED AND REFUSED ON THE RECORD (1/3): the counter-argument is the most sympathetic these gates have faced because it is a SECURITY argument rather than a capability one — every side-effecting tool call now passes through a human's button, the screen is computed rather than narrated, the run parks, the question expires, the approver is a project policy, so `slack` should be `stable`. It is refused first because §6 LEG 1 IS STILL OPEN AND E23 GREW IT: the leg is a CAPTURED, re-derivable receipt from a real Slack workspace, and its scope now includes the approval message, the modal, an unauthorized click and a merge. ToolApprovalPeer is structurally the literal \"fake\". When a leg grows you are further from the flip, not nearer",
			"TIER DECISION (2/3), AND IT IS THE RULE RATHER THAN THE CIRCUMSTANCE: ADDING A CONTROL IS NOT EVIDENCE THAT THE CONTROL WORKS IN A REAL WORKSPACE. E22 did not advance a tier for DELETING a boundary; E23 does not advance one for ADDING a boundary — and the symmetry is the whole argument, because both rest on the same fake peer and what this gate measures is evidence rather than claims. A release that promoted itself for installing a control would be certifying its own intentions",
			"TIER DECISION (3/3): T5's CEILING IS OPEN BY CONSTRUCTION. A misconfigured registration — an MCP write tool published without `approval_required` — silently skips the gate, no warning is printed and nothing in the tree detects it. It cannot be otherwise: automatic classification would have to trust a server's own annotations, which §3.5 P3 forbids in the vendor's own words. A control with a silent bypass is not a control a tier is promoted on, and the bypass is filed as HIL-P5 rather than smoothed over",
			"WHAT WOULD HAVE HAD TO BE TRUE to move `slack` to `stable`: (i) a CAPTURED, re-derivable receipt from a real workspace; (ii) the removal of Peer's structural \"fake\" constraint; (iii) §6 leg 1 green in ONE run across E19's six legs, E20's four, E21's, E22's and E23's. None of the three exists",
			"AND THE THING THIS RELEASE REFUSES TO LET A READER MISS, BECAUSE IT IS THIS EPIC'S OWN D1 AND E22 SHIPPED THE SAME SHAPE ONE LAYER DOWN: THE GENERIC HALF HAS NO PRODUCTION DECISION SURFACE. `slack.ToolApprovalMessage` — the three-button argument table T4 built — has NO production caller; the shipped pump posts E19's two-button `slack.ApprovalMessage` for PUBLICATIONS. `coordinator.DecideToolApproval` has NO production caller either; a Slack click decides a publication through ApplyApprovalDecision. So a gated MCP tool call today PARKS ITS RUN AND IS RELEASED BY THE EXPIRY REAPER, and the approved Jira transition this bundle certifies was decided by a component test calling the decision function directly. Measured against the tree 2026-07-29 and filed as HIL-P8. What IS production is the gate itself (nothing runs without a decision), the park, the expiry-and-wake, the approver check, the modal read path and the whole publication line",
			"MIGRATION: 000044_tool_approvals is this epic's ONLY migration and the chain stops there. Four riders: `approvals` became operation-agnostic (the table's OWN 2023 comment ordered this epic by name — \"a second producer … is when this grows an operation-agnostic target\"), `publications.operation` gained 'merge_pull_request' (merge is a CHECK value, not a code change), `tool_revisions.approval_required`, and `slack_approval_deliveries` — a NEW table rather than a loosened one, because `slack_reply_deliveries` is UNIQUE (run_id) and a run can produce more than one approval, so reusing it would have moved a shipped exactly-once claim",
			"THREE VENDOR QUESTIONS ARE UNANSWERED AND ARE NOT IN THE CONTRACT LEDGER, WHICH IS THE HONEST ANSWER: §3.5 P5 (whether MCP 2025-11-25 actually defines ToolAnnotations — the field is named, its contents are not enumerated on either page), P17 (whether `interactivity_pointer` shares trigger_id's three seconds, and whether Slack accepts a views.open concurrent with the ack), P18 (a view_submission payload's documented maximum and private_metadata's limit). None entered the code as an assumption: annotations are not decoded at all, `interactivity_pointer` is never used, and private_metadata carries two short strings. Each is a row in docs/operations/known-gaps-1.0.md and each is a §6 measurement",
		},
		"tool_approval_claim": "every-side-effecting-tool-call-passes-through-a-humans-button-who-reads-the-call-itself-and-the-run-parks-without-waiting-forever",
		"tool_approval_proof": canonicalToolApprovalProof(),
	}
	approvalCase["checksum"] = hashParts(approvalAnchorCaseID, approvalCase["run_id"].(string), anchor)
	cases = append(cases, approvalCase)
	outcomes[approvalAnchorCaseID] = "PASS"

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
			SnapshotSource: "carried forward from " + baselineManifest + ", which carried it from the E21 bundle and before that from the E19 wiring bundle, where it was read over real HTTP from a FULLY MOUNTED router. E23 mounts one new interactivity branch (the Show-arguments modal) on a route that was already mounted, opens no capability and moves no tier, so the snapshot it would take is the snapshot those releases already earned — and the promote gate re-derives the comparison from committed bytes rather than trusting this sentence",
			ClaimsDigest:   uat.CapabilityClaimsDigest(),
		}
	}

	manifest := map[string]any{
		"release":     uat.ToolApprovalBundle,
		"git_sha":     "e09eec6",
		"api_version": "v1",
		"migration":   "000044_tool_approvals",
		"captured_at": "2026-07-29T00:00:00Z",
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

// canonicalToolApprovalProof is the proof body the bundle carries. Every BYTE field is READ FROM A CANONICAL
// SOURCE rather than retyped — the contract ledger from uat.ToolApprovalContracts, the approval screen from
// the SHIPPED renderers via uat.ToolApprovalScreen, and the mint file list RECOMPUTED from the source of two
// packages. So nothing here can drift away from what the code produces.
func canonicalToolApprovalProof() uat.ToolApprovalProof {
	screen := uat.ToolApprovalScreen()
	minted, err := uat.SweepActionableElements(screen)
	if err != nil {
		panic("the approval screen cannot be swept, so this bundle cannot be written: " + err.Error())
	}
	mints, err := uat.SweepActionableElementMints()
	if err != nil {
		panic("the actionable-element mints cannot be re-derived, so this bundle cannot be written: " + err.Error())
	}
	files := make([]string, 0, len(mints))
	for file := range mints {
		files = append(files, file)
	}
	sortStrings(files)
	return uat.ToolApprovalProof{
		Peer: uat.ToolApprovalPeer,
		// Three gated calls a human approved and that ran, one denied and withheld, one nobody answered and
		// the reaper cancelled, two ungated calls that ran untouched.
		CallLedger:               json.RawMessage(uat.ToolApprovalCallLedger),
		SideEffectsWithoutAHuman: 0,

		ApprovalScreen:                       screen,
		UntrustedTextNeedles:                 uat.ToolApprovalUntrustedNeedles,
		UntrustedTextArrived:                 json.RawMessage(uat.ToolApprovalTextArrivedFromOutside),
		ScreenCharactersFromTheModelOrServer: 0,

		RunLedger:                json.RawMessage(uat.ToolApprovalRunLedger),
		RunsTerminalWhileWaiting: 0,
		RunsWaitingAfterExpiry:   0,

		DecisionLedger:               json.RawMessage(uat.ToolApprovalDecisionLedger),
		UnauthorizedDecisionsApplied: 0,

		PublishToolSchemas:      json.RawMessage(uat.ToolApprovalPublishSchemas),
		ModelChosenDestinations: 0,

		// The count comes from the sweep over the rendered screen, never from a literal: three buttons, each
		// with an action_id, a value and a `button` type, one `actions` block, and the modal's deny-reason
		// input element.
		ActionableElementsMinted:   len(minted),
		ActionableElementMintFiles: files,

		Contracts:       uat.ToolApprovalContracts,
		ContractsDigest: uat.ToolApprovalContractsDigest(),
	}
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
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

// TestCommittedToolApprovalBundleIsTheGeneratorOutput pins the committed bundle to the tree: it must be
// BYTE-identical to this generator's output, so a contract-ledger change, a renderer change (the approval
// screen comes from the SHIPPED renderers), a mint-site change, a case-set change or a tier change cannot
// leave a stale bundle verifying green.
// Regenerate with: PALAI_WRITE_TOOL_APPROVAL_BUNDLE=1 go test ./tests/uat/tool-approval/
func TestCommittedToolApprovalBundleIsTheGeneratorOutput(t *testing.T) {
	want := buildToolApprovalManifest(t)
	path := filepath.Join(bundleDir(t), "manifest.json")

	if os.Getenv("PALAI_WRITE_TOOL_APPROVAL_BUNDLE") == "1" {
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
		t.Fatalf("read the committed bundle: %v (regenerate with PALAI_WRITE_TOOL_APPROVAL_BUNDLE=1)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("the committed %s bundle is not this generator's output — regenerate with PALAI_WRITE_TOOL_APPROVAL_BUNDLE=1", uat.ToolApprovalBundle)
	}
}

// TestToolApprovalReleaseVerifiesClean runs the committed bundle through the SHIPPED verifier and requires
// 0 failed / 0 missing / 0 secret. It is the `make evidence-verify RELEASE=tool-approval-0.1.0` gate,
// in-process.
func TestToolApprovalReleaseVerifiesClean(t *testing.T) {
	summary, err := uat.VerifyRelease(bundleDir(t), nil)
	if err != nil {
		t.Fatalf("verify the tool-approval release: %v", err)
	}
	if !summary.OK() {
		t.Fatalf("the tool-approval bundle did not verify clean: %s\n%v", summary.String(), summary.Findings)
	}
	if summary.Passed == 0 {
		t.Fatal("the tool-approval bundle verified 0 passed cases — a zero-case bundle is not a clean bundle")
	}
	t.Logf("%s: %s", uat.ToolApprovalBundle, summary.String())
}

// TestTheFiveNewCasesAreInTheBundle is the shrink guard the E18 T8 sweep taught: a release that OPENED five
// ids must carry all five, or the ids exist in the tree with no bundle certifying them.
func TestTheFiveNewCasesAreInTheBundle(t *testing.T) {
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
	for _, id := range uat.ToolApprovalCaseIDs {
		if status, ok := carried[id]; !ok {
			t.Errorf("%s is a case this epic OPENED but the bundle that certifies it does not carry it", id)
		} else if status != "PASS" {
			t.Errorf("%s is carried as %q; this release claims it green", id, status)
		}
	}
	for _, anchorID := range []string{approvalAnchorCaseID, shipAnchorCaseID, memoryAnchorCaseID, surfaceAnchorCaseID, tierAnchorCaseID} {
		if _, ok := carried[anchorID]; !ok {
			t.Errorf("the bundle carries no %s anchor — a release that dropped an inherited anchor would be judged by a gate that never ran", anchorID)
		}
	}
}

// TestToolApprovalBundleNeverClaimsARealWorkspaceOrARealMerge is the honest-ceiling guard, and it is
// deliberately about the TEXT rather than the proof struct: Complete() already refuses a Peer other than
// "fake", so what remains to catch is prose that overclaims around a mechanically-honest proof — the way an
// evidence bundle actually misleads a reader.
//
// The scan runs PER SENTENCE and a sentence carrying a negation marker is the honest form, because this
// bundle's own tier-decision paragraphs must NAME the real workspace and the real merge in order to refuse
// them.
func TestToolApprovalBundleNeverClaimsARealWorkspaceOrARealMerge(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(bundleDir(t), "manifest.json"))
	if err != nil {
		t.Fatalf("read the bundle: %v", err)
	}
	negationMarkers := []string{"§6 leg", "never", "not ", "no ", "nothing", "refus", "untouched", "unmet",
		"fake", "preview", "disabled", "would have", "cannot"}
	for _, sentence := range strings.Split(string(raw), ". ") {
		lower := strings.ToLower(sentence)
		for _, forbidden := range []string{"real slack workspace", "real workspace receipt", "in production",
			"verified in slack", "real github app", "real atlassian", "real merge", "merged pull request"} {
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
				t.Errorf("the bundle names %q in a sentence that does NOT negate it — this release contacted no workspace, no Atlassian tenant and no GitHub App, and may not imply one:\n  %s", forbidden, strings.TrimSpace(sentence))
			}
		}
	}
	// The ceilings a reader must meet IN THE MANIFEST rather than only in this gate's comments. The last two
	// are this epic's own D1 — the half a summary would drop.
	for _, required := range []string{"§6 leg 1", "§6 leg 6", "\"fake\"", "NO PRODUCTION DECISION SURFACE",
		"EXPIRY REAPER", "HIL-P8", "silently skips the gate", "MIGRATION: 000044_tool_approvals"} {
		if !strings.Contains(string(raw), required) {
			t.Errorf("the bundle never mentions %q — the ceiling has to be legible in the manifest itself, not only in this gate's comments", required)
		}
	}
}

// TestToolApprovalBundleCarriesEveryDivergenceRow is the §3.5 completeness check. The plan's crown output is
// the divergence table, so an epic that implemented a surface while silently dropping the row that named its
// gap would be exactly the regression this family exists to prevent.
func TestToolApprovalBundleCarriesEveryDivergenceRow(t *testing.T) {
	seen := map[string]bool{}
	for _, req := range uat.ToolApprovalContracts {
		seen[req.Divergence] = true
		if req.SourceURL == "" || req.Requirement == "" {
			t.Errorf("divergence %s carries no source or no requirement text — a requirement nobody can audit is not grounding", req.Divergence)
		}
		if !strings.HasPrefix(req.SourceURL, "http") && !strings.Contains(req.SourceURL, "MEASURED") {
			t.Errorf("divergence %s is grounded in neither a URL nor a MEASURED stamp: %q", req.Divergence, req.SourceURL)
		}
		// Every row carries the DATE it was read. An undated citation cannot be re-checked, which is how a
		// vendor page that changed under a claim stays invisible.
		if !strings.Contains(req.SourceURL, "2026-") {
			t.Errorf("divergence %s cites a source with no fetch date — an undated citation cannot be re-read: %q", req.Divergence, req.SourceURL)
		}
	}
	// P1..P4, P6..P16 are the plan §3.5 rows E23 implements or acted on. P5/P17/P18 are checked separately
	// and must be ABSENT.
	for _, row := range []string{"P1", "P2", "P3", "P4", "P6", "P7", "P8", "P9", "P10", "P11", "P12",
		"P13", "P14", "P15", "P16"} {
		if !seen[row] {
			t.Errorf("§3.5 row %s is not in the contract ledger — the divergence table is the plan's crown output and a dropped row is a silently reintroduced gap", row)
		}
	}
}

// TestTheToolApprovalLedgerRefusesTheUnconfirmedRows pins the three rows that must NOT be there. P5, P17 and
// P18 are things no published page states; putting them in a ledger of "published requirements this surface
// implements" would dress three unknowns as three citations. Their home is
// docs/operations/known-gaps-1.0.md, and TestTheE23UncertaintiesAreInKnownGaps checks they arrived.
func TestTheToolApprovalLedgerRefusesTheUnconfirmedRows(t *testing.T) {
	for _, req := range uat.ToolApprovalContracts {
		switch req.Divergence {
		case "P5", "P17", "P18":
			t.Errorf("the contract ledger carries %s (%q) — that row is a question the DOCUMENTATION DOES NOT ANSWER, and a ledger entry would give it a source URL it does not have", req.Divergence, req.Requirement)
		}
	}
}
