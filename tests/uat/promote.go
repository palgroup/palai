package uat

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/palgroup/palai/tests/evals"
)

// Refusal is one reason a release cannot be tagged/promoted. An empty slice from PromoteGate is a clean pass.
type Refusal struct{ Detail string }

func (r Refusal) String() string { return r.Detail }

// PromoteGate is the mechanical form of the SH-2 exit-gate sentence (plan §5, §7): "a release without
// rollback/restore proof cannot be promoted." It refuses to tag a release whose bundle does not carry a
// COMPLETE UpgradeProof — with BOTH the app + engine-alias rollback and the drain-before-recreate invariant
// (T2 MF-3) — AND at least one restore/DR proof (a BackupProof, a RestoreVerifyProof, or a DrillProof).
//
// A promote BEYOND rc (target == "stable") ALSO awaits the E14 §6 operator legs 1-2 (a real cloud-VM clean
// install + a separate-host restore), tracked as an operator_attestation note in the manifest. That note is
// NEVER auto-claimed here — when it is absent, the beyond-rc promote is REFUSED, so the gate can never assert
// the operator legs ran when they did not. The rc promote itself does not require the note (the local seam is
// the RC proof), only the beyond-rc promote does.
func PromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	hasRollback, hasRestore := false, false
	for _, c := range m.Cases {
		if c.UpgradeClaim != "" && c.UpgradeProof != nil && c.UpgradeProof.Complete() &&
			c.UpgradeProof.AppRollback && c.UpgradeProof.EngineAliasRollback && c.UpgradeProof.RollbackDrained {
			hasRollback = true
		}
		if (c.BackupClaim != "" && c.BackupProof != nil && c.BackupProof.Complete()) ||
			(c.RestoreVerifyClaim != "" && c.RestoreVerifyProof != nil && c.RestoreVerifyProof.Complete()) ||
			(c.DrillClaim != "" && c.DrillProof != nil && c.DrillProof.Complete()) {
			hasRestore = true
		}
	}

	var refusals []Refusal
	if !hasRollback {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE UpgradeProof with app + engine-alias rollback and the drain-before-recreate invariant (T2 MF-3) — a release without rollback proof cannot be promoted (plan §7 exit gate)"})
	}
	if !hasRestore {
		refusals = append(refusals, Refusal{Detail: "no restore/DR proof (a BackupProof, a RestoreVerifyProof, or a DrillProof) — a release without restore proof cannot be promoted (plan §7 exit gate)"})
	}
	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the E14 §6 operator legs 1-2 (a real cloud-VM clean install + a separate-host restore); no operator_attestation in the manifest — this note is never auto-claimed (plan §6, §T6)"})
	}
	return refusals
}

// PromoteGateFor dispatches to the release-family promote gate: a bundle carrying E16 SDK-parity claims
// (three_language_equality / gateway_off) is gated by SDKParityPromoteGate; a bundle carrying E15 upgrade claims
// is gated by PromoteGate. A bundle with neither is refused — there is no promote policy for it, so the promote
// command cannot silently pass a release no gate recognizes. This lets one `make promote` entry serve both
// families without coupling their rules.
func PromoteGateFor(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}
	// The Faz A.3 tool-execution family is checked FIRST, ahead of E28, and for the reason every clause below
	// repeats one level down: it is now the most specific policy in the tree and it COMPOSES the fleet-console
	// gate underneath itself (which composes background, admin-console, tool-approval, code-and-ship,
	// tools-memory, agent-surface, wiring, the E17 tier table and the eval gate). An A.3 bundle DERIVES its
	// inherited case set from the E28 release, so it also carries the E28 fleet-console claim, the E26
	// background claim, the E25 admin-console claim, the E23 tool-approval claim and everything under them —
	// without this clause it would reroute to FleetConsolePromoteGate, which knows nothing about the placement
	// ledger, the no-fallback half, the background addressing, the outstanding `uname` legs or the SUPERSEDED
	// published ceilings, and would pass it: every tool-execution guard would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE A.3 CASE IDS, NOT BY THE tool_execution_claim THIS GATE ENFORCES — the
	// ninth time this reasoning has been written down, and the reason it keeps being written is that the
	// alternative fails SILENTLY. That is also why A.3's ids carry the `EXE-` prefix: `FLT-`, `CON-`, `BGT-`
	// and `FLC-` are each already inside extensionIDPrefixes with their own owner lists and their own gates,
	// so an `FLT-006` would match carriesE24FleetCase four clauses down and dispatch to FleetPromoteGate — a
	// WEAKER gate for this bundle, which is the promote-gate-family-dispatch defect reached from a naming
	// choice. TestTheA3AndE2xFamiliesAreDisjoint in tests/uat/tool-execution asserts the id sets never overlap
	// rather than leaving it assumed.
	for _, c := range m.Cases {
		if carriesA3ToolExecutionCase(c) {
			return ToolExecutionPromoteGate(raw, target)
		}
	}
	// The E28 fleet-console family is checked next, ahead of E26, and for the reason every clause below
	// repeats one level down: it is now the most specific policy in the tree and it COMPOSES the background
	// gate underneath itself (which composes admin-console, tool-approval, code-and-ship, tools-memory,
	// agent-surface, wiring, the E17 tier table and the eval gate). An E28 bundle DERIVES its inherited case
	// set from the E26 release, so it also carries the E26 background claim, the E25 admin-console claim, the
	// E23 tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim, the E20 agent-surface
	// claim and E17 area claims — without this clause it would reroute to BackgroundPromoteGate, which knows
	// nothing about the pool birth path, the waiting room admitted from a screen, the minted-value scan, the
	// approver-list equality, the axe coverage, the confirmation split or the ceiling ids, and would pass it:
	// every fleet console guard would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E28 CASE IDS, NOT BY THE fleet_console_claim THIS GATE ENFORCES. That
	// is also why E28's ids carry the `FLC-` prefix and NOT `FLT-` or `CON-`: both of those are already
	// inside extensionIDPrefixes with their own owner lists and their own gates, so an `FLT-006` would match
	// carriesE24FleetCase two clauses down and dispatch to FleetPromoteGate — a WEAKER gate for this bundle,
	// which is the promote-gate-family-dispatch defect reached from a naming choice. TestTheE2xFamiliesAreDisjoint
	// in tests/uat/fleet-console asserts the id sets never overlap rather than leaving it assumed.
	for _, c := range m.Cases {
		if carriesE28FleetConsoleCase(c) {
			return FleetConsolePromoteGate(raw, target)
		}
	}
	// The E26 background family is checked next, ahead of E25, and for the reason every clause below repeats
	// one level down: it is now the most specific policy in the tree and it COMPOSES the admin-console gate
	// underneath itself (which composes tool-approval, code-and-ship, tools-memory, agent-surface, wiring, the
	// E17 tier table and the eval gate). An E26 bundle DERIVES its inherited case set from the E25 release, so
	// it also carries the E25 admin-console claim, the E23 tool-approval claim, the E22 code-and-ship claim,
	// the E21 tools-memory claim, the E20 agent-surface claim and E17 area claims — without this clause it
	// would reroute to AdminConsolePromoteGate, which knows nothing about the six replicated semantics, the
	// refusal controls, the two ownership postures, the exactly-once notice, the reaper's duties or the
	// redaction sites, and would pass it: every background guard would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E26 CASE IDS, NOT BY THE background_claim THIS GATE ENFORCES. That is
	// also why E26's ids carry the `BGT-` prefix: an id already inside AdminConsoleCaseIDs, FleetCaseIDs,
	// ToolApprovalCaseIDs, CodeAndShipCaseIDs, ToolsMemoryCaseIDs or AgentSurfaceCaseIDs would have matched an
	// EARLIER family marker and dispatched to a WEAKER gate, and a `SLK-`/`WRK-`/`UI-` id outside all of them
	// would have regenerated a shipped bundle — the promote-gate-family-dispatch defect, reachable from a
	// naming choice in two directions.
	for _, c := range m.Cases {
		if carriesE26BackgroundCase(c) {
			return BackgroundPromoteGate(raw, target)
		}
	}
	// The E25 admin-console family is checked next, ahead of E24 and E23, and for the reason every clause
	// below repeats one level down: it is now the most specific policy in the tree and it COMPOSES the
	// tool-approval gate underneath itself (which composes code-and-ship, tools-memory, agent-surface,
	// wiring, the E17 tier table and the eval gate). An E25 bundle DERIVES its inherited case set from the
	// E23 release, so it also carries the E23 tool-approval claim, the E22 code-and-ship claim, the E21
	// tools-memory claim, the E20 agent-surface claim and E17 area claims — without this clause it would
	// reroute to ToolApprovalPromoteGate, which knows nothing about the relay-gate totality re-derivation,
	// the axe coverage equality, the ciphertext query pin, the byte scan, the sweep floor or the ledger
	// repairs, and would pass it: every console guard would be optional in practice.
	//
	// IT COMPOSES E23 AND NOT E24, AND THAT IS A MEASUREMENT. E25 ran in PARALLEL with E24 (plan §7) and
	// inherits from `tool-approval-0.1.0`, not from `runner-fleet-0.1.0`: it built nothing on the fleet and
	// carries no `FLT-` case. So the two families are SIBLINGS with disjoint id sets, the relative order of
	// this clause and the E24 clause below is immaterial, and TestTheE24AndE25FamiliesAreDisjoint asserts
	// that rather than leaving it to be assumed — because "immaterial today" is how a dispatch order becomes
	// load-bearing tomorrow.
	//
	// THE FAMILY IS RECOGNIZED BY THE E25 CASE IDS, NOT BY THE admin_console_claim THIS GATE ENFORCES. That
	// is also why E25's ids carry the `CON-` prefix: a `UI-005` would be inside extensionIDPrefixes, whose
	// map IS the shipped extensions-0.1.0 bundle's case list, so it would either regenerate a committed
	// historical release or fall through to a WEAKER gate — the promote-gate-family-dispatch defect,
	// reachable from a naming choice in two directions.
	for _, c := range m.Cases {
		if carriesE25AdminConsoleCase(c) {
			return AdminConsolePromoteGate(raw, target)
		}
	}
	// The E24 fleet family is checked next, ahead of E23, and for the reason every clause below repeats one
	// level down: it is now the most specific policy in the tree and it COMPOSES the tool-approval gate
	// underneath itself (which composes code-and-ship, tools-memory, agent-surface, wiring, the E17 tier
	// table and the eval gate). An E24 bundle DERIVES its inherited case set from the E23 release, so it also
	// carries the E23 tool-approval claim, the E22 code-and-ship claim, the E21 tools-memory claim, the E20
	// agent-surface claim and E17 area claims — without this clause it would reroute to
	// ToolApprovalPromoteGate, which knows nothing about the wrong-pool sweep, the cross-tenant sweep, the
	// capacity-death count, the key-revocation fence or the server-mint recompute, and would pass it: every
	// fleet guard would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E24 CASE IDS, NOT BY THE fleet_claim THIS GATE ENFORCES. That is also
	// why E24's ids carry the `FLT-` prefix: an id already inside CodeAndShipCaseIDs, ToolsMemoryCaseIDs,
	// AgentSurfaceCaseIDs or ToolApprovalCaseIDs would have matched an EARLIER family marker and dispatched
	// to a WEAKER gate, and a `WRK-`/`SAN-`/`OPS-` id outside all four would have regenerated a shipped
	// bundle — the promote-gate-family-dispatch defect, reachable from a naming choice in two directions.
	for _, c := range m.Cases {
		if carriesE24FleetCase(c) {
			return FleetPromoteGate(raw, target)
		}
	}
	// The E23 tool-approval family is checked next, ahead of E22, and for the reason every clause below
	// repeats one level down: it is now the most specific policy in the tree and it COMPOSES the
	// code-and-ship gate underneath itself (which composes tools-memory, agent-surface, wiring, the E17 tier
	// table and the eval gate). An E23 bundle DERIVES its inherited case set from the E22 release, so it
	// also carries the E22 code-and-ship claim, the E21 tools-memory claim, the E20 agent-surface claim and
	// E17 area claims — without this clause it would reroute to CodeAndShipPromoteGate, which knows nothing
	// about the ungoverned-side-effect sweep, the screen-authorship fence, the park/expiry counts or the
	// single-mint recompute, and would pass it: all four crown guards would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E23 CASE IDS, NOT BY THE tool_approval_claim THIS GATE ENFORCES. That
	// is also why E23's ids carry the `HIL-` prefix: an id already inside CodeAndShipCaseIDs,
	// ToolsMemoryCaseIDs or AgentSurfaceCaseIDs would have matched an EARLIER family marker and dispatched
	// to a WEAKER gate, and an `SLK-` id outside all three would have regenerated a shipped bundle — the
	// promote-gate-family-dispatch defect, reachable from a naming choice in two different directions.
	for _, c := range m.Cases {
		if carriesE23ToolApprovalCase(c) {
			return ToolApprovalPromoteGate(raw, target)
		}
	}
	// The E22 code-and-ship family is checked next, ahead of E21: it is the next most specific policy in the
	// tree and it COMPOSES the tools-memory gate underneath itself (which composes agent-surface, which
	// composes wiring, which composes the E17 tier table, which composes the eval gate). An E22 bundle also
	// carries the E21 tools-memory claim, the E20 agent-surface claim and the E19 wiring claim — it DERIVES
	// its case set from those releases — so without this clause it would reroute to ToolsMemoryPromoteGate,
	// which knows nothing about the unapproved-publication sweep, the destination sweep or the typed-operation
	// ceiling, and would pass it: all three crown guards would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E22 CASE IDS, NOT BY THE code_and_ship_claim THIS GATE ENFORCES. This is
	// also why E22's ids carry the `CAS-` prefix rather than `SLK-` or `TLM-`: an id inside AgentSurfaceCaseIDs
	// or ToolsMemoryCaseIDs would have matched an EARLIER family marker and dispatched to a WEAKER gate, and
	// an `SLK-` id outside both would have regenerated a shipped bundle — the promote-gate-family-dispatch
	// defect, reachable from a naming choice in two different directions.
	for _, c := range m.Cases {
		if carriesE22CodeAndShipCase(c) {
			return CodeAndShipPromoteGate(raw, target)
		}
	}
	// The E21 tools-and-memory family is checked next, ahead of E20: it is the next most specific policy in
	// the tree and it COMPOSES the agent-surface gate underneath itself (which composes wiring, which
	// composes the E17 tier table, which composes the eval gate). An E21 bundle also carries the E20
	// agent-surface claim and the E19 wiring claim — it DERIVES its case set from those releases — so without
	// this clause it would reroute to AgentSurfacePromoteGate, which knows nothing about the stored-search-byte
	// or mention re-derivations and would pass it: both crown guards would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E21 CASE IDS, NOT BY THE tools_memory_claim THIS GATE ENFORCES. This is
	// also why E21's ids carry the `TLM-` prefix rather than `SLK-`: an `SLK-013` outside AgentSurfaceCaseIDs
	// would have matched no family marker at all and fallen through to a WEAKER gate — the
	// promote-gate-family-dispatch defect, arrived at by a naming choice.
	for _, c := range m.Cases {
		if carriesE21ToolsMemoryCase(c) {
			return ToolsMemoryPromoteGate(raw, target)
		}
	}
	// The E20 agent-surface family is checked next, ahead of E19: it is the most specific policy in the
	// tree and it COMPOSES the wiring gate underneath itself (which composes the E17 tier table, which
	// composes the eval gate). An agent-surface bundle also carries the E19 wiring claim — it derives its
	// case set from that release — so without this clause it would reroute to WiringPromoteGate, which knows
	// nothing about the forgery derivation and would pass it: the crown guard would be optional in practice.
	//
	// THE FAMILY IS RECOGNIZED BY THE E20 CASE IDS, NOT BY THE agent_surface_claim THIS GATE ENFORCES.
	// Dispatching on the claim the gate enforces is exactly how a release drops it and reroutes to a weaker
	// family that passes — the defect this file's E17 comment describes.
	for _, c := range m.Cases {
		if carriesE20AgentSurfaceCase(c) {
			return AgentSurfacePromoteGate(raw, target)
		}
	}
	// The E19 wiring family is checked next, ahead of E18: it is the most specific policy in the tree (it
	// governs a single epic's mounts AND composes the E17 tier table underneath it), and — the rule that
	// matters — the family is recognized by the wiring claim while the gate ENFORCES the mount derivation.
	// A wiring bundle also carries E17 area claims, so without this clause it would reroute to
	// ExtensionsPromoteGate, which knows nothing about mounts and would pass it: the mount guard would be
	// optional in practice. That is the exact failure the E17 dispatch comment below describes.
	for _, c := range m.Cases {
		if carriesE19WiringClaim(c) {
			return WiringPromoteGate(raw, target)
		}
	}
	// The E18 stable-release family is checked next: among the remaining policies it is the only one that
	// governs the whole product rather than one epic. Recognized by ANY E18 area claim, never
	// by the release index or the aggregate posture themselves — dispatching on the claim the gate enforces
	// is precisely how a release DROPS it, reroutes to a weaker family and passes.
	for _, c := range m.Cases {
		if carriesE18AreaClaim(c) {
			return StableReleasePromoteGate(raw, target)
		}
	}
	// The E17 extensions bundle carries BOTH the capability-tier anchor and the eval-gate claim; the tier
	// anchor is the more specific policy and COMPOSES the eval gate, so it is checked first.
	//
	// Crucially the family is recognized by ANY E17 area claim, NOT by the tier claim itself. Dispatching on
	// the tier claim alone would let a release DROP its tier table and fall through to the weaker eval gate,
	// which would then pass it — the tier table would be optional in practice. Recognizing the family first and
	// REFUSING the missing table inside ExtensionsPromoteGate is what makes "no tag without the tier table"
	// actually hold. carriesE17AreaClaim is shared with the manifest verifier so the two surfaces cannot drift
	// about what an E17 release is.
	for _, c := range m.Cases {
		if carriesE17AreaClaim(c) {
			return ExtensionsPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.EvalGateClaim != "" {
			return EvalPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.ThreeLanguageEqualityClaim != "" || c.GatewayOffClaim != "" {
			return SDKParityPromoteGate(raw, target)
		}
	}
	for _, c := range m.Cases {
		if c.UpgradeClaim != "" {
			return PromoteGate(raw, target)
		}
	}
	return []Refusal{{Detail: "no promote policy for this release: it carries none of the E21 tools-and-memory, E20 agent-surface, E19 wiring, E18 stable-release, E17 extensions/eval-gate, E16 SDK-parity or E15 upgrade claims a promote gate recognizes"}}
}

// AgentSurfacePromoteGate is the mechanical form of the E20 exit-gate sentence (plan §T5, §7): a release
// cannot be tagged while the FORGERY guard is red. It has three clauses:
//
//  1. the bundle must carry EXACTLY ONE complete SlackAgentSurfaceProof, and the actionable-element count is
//     RE-DERIVED here from the closing blocks the proof carries — this gate does its own sweep rather than
//     inheriting Complete()'s verdict (the E15 SF-4 shape: the gate that would tag a release derives for
//     itself). A closing message carrying so much as one `action_id` outside the approval builder REFUSES
//     the tag, because a model-minted button is indistinguishable to a human from our approval button and
//     passes through none of ApproverAuthorized → AcceptCommand → ApplyApprovalDecision;
//  2. NO TIER MAY ADVANCE — enforced by the composed wiring gate against the committed E17 baseline. E20 is
//     the epic with the STRONGEST claim to a tier bump and the weakest right to one: a real workspace is
//     connected and a real run completed on 2026-07-26, but that run left NO CAPTURED RECEIPT, it
//     demonstrated ONE claim rather than the twelve this release carries, and E20 GREW the surface more than
//     any epic before it. Growing a surface is the moment a tier is least earned;
//  3. the E19 wiring gate is COMPOSED verbatim — the mount derivation, the cross-bundle tier comparison, the
//     E17 tier table and the eval numbers underneath it. Composing rather than re-deriving keeps ONE owner
//     of each rule.
//
// A promote BEYOND rc inherits every refusal underneath and adds nothing of its own: no amount of surface
// promotes this release, because `slack` caps at preview by construction (CapabilityOperatorLegs §6 leg 1)
// and this epic contacted no workspace.
func AgentSurfacePromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var surface *SlackAgentSurfaceProof
	claims := 0
	for _, c := range m.Cases {
		if c.AgentSurfaceClaim == "" {
			continue
		}
		claims++
		if surface == nil {
			surface = c.AgentSurfaceProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d agent_surface_claims in one manifest (want exactly 1): this gate judges the FIRST agent-surface proof, so a second could ride behind an honest one — one release, one forgery derivation (plan §T5)", claims)})
	case surface == nil:
		refusals = append(refusals, Refusal{Detail: "no COMPLETE SlackAgentSurfaceProof (one visible message per run + the three admission entrances through the ONE Admit + the transport-invariance counter + a context that granted zero authority + zero actionable elements minted outside the approval builder + the canonical vendor contract ledger) — a release whose forgery guard is red cannot be tagged (plan §T5, §7 exit gate)"})
	default:
		// THE GATE'S OWN SWEEP RUNS FIRST, before Complete() is consulted, and the ORDER is the whole point.
		// Complete() re-derives the same count, so putting it ahead would make this branch unreachable for
		// every input it exists to catch — a guard that cannot fire, which is this repository's most-found
		// defect. Running it first means a tag decision rests on THIS gate's derivation (the E15 SF-4 shape)
		// and the reader is told WHICH claim failed rather than that something did.
		found, err := SweepActionableElements(surface.ClosingBlocks)
		switch {
		case err != nil:
			refusals = append(refusals, Refusal{Detail: "the agent-surface proof's closing blocks cannot be swept, so the forgery count is unverifiable and the tag is REFUSED (fail closed): " + err.Error()})
		case len(found) != 0:
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf(
				"the closing message carries %d actionable element(s) minted outside interactions.go: %v — the model may never mint something a human can press, and a bundle declaring %d over these bytes is fabricated (plan §2, §T5)",
				len(found), found, surface.ActionableElementsOutsideApprovalBuilder)})
		case surface.ApprovalBuilderMints < 1:
			refusals = append(refusals, Refusal{Detail: "the agent-surface proof reports ZERO approval-builder mints, so the forgery sweep never demonstrated it could FIND an actionable element — a guard that has never found one is not a guard (plan §T5)"})
		}
		if !surface.Complete() {
			refusals = append(refusals, Refusal{Detail: "no COMPLETE SlackAgentSurfaceProof (one visible message per run + the three admission entrances through the ONE Admit + the transport-invariance counter + a context that granted zero authority + zero actionable elements minted outside the approval builder + the canonical vendor contract ledger) — a release whose forgery guard is red cannot be tagged (plan §T5, §7 exit gate)"})
		}
	}

	return append(refusals, WiringPromoteGate(raw, target)...)
}

// WiringPromoteGate is the mechanical form of the E19 exit-gate sentence (plan §T9, §7): a release cannot be
// tagged while the MOUNT-DERIVATION guard is red. It has three clauses:
//
//  1. the bundle must carry EXACTLY ONE complete WiringProof, and every surface's declared mount must
//     RE-DERIVE from the running stack's own router surface and /v1/capabilities snapshot. The recompute
//     lives here as well as in VerifyManifest deliberately: the gate that would tag a release must do its
//     own derivation, never inherit a verdict (the E15 SF-4 shape);
//  2. NO TIER MAY ADVANCE. E19's output is wiring, document-accuracy and ready-to-run live legs — never tier
//     movement — so a wiring bundle that recomputes a capability to `stable` which the §6 operator legs
//     still cap at `preview` is REFUSED here rather than being explained away in prose. This is the clause
//     that makes "wiring does not promote" a mechanism instead of a promise;
//  3. the E17 extensions gate is COMPOSED verbatim — the tier table, the QUA-003 security precondition and
//     the recomputed eval numbers underneath it. Composing (rather than re-deriving) keeps ONE owner of the
//     tier rule, exactly as ExtensionsPromoteGate composes EvalPromoteGate.
//
// A promote BEYOND rc inherits every refusal underneath and adds nothing of its own: no amount of wiring
// promotes this release, because `slack`, `a2a`, `queues` and `console` cap at preview by construction
// (CapabilityOperatorLegs) and this epic contacted none of their counterparties.
func WiringPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	var wiring *WiringProof
	claims := 0
	for _, c := range m.Cases {
		if c.WiringClaim == "" {
			continue
		}
		claims++
		if wiring == nil {
			wiring = c.WiringProof
		}
	}
	switch {
	case claims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d wiring_claims in one manifest (want exactly 1): this gate judges the FIRST wiring proof, so a second table could ride behind an honest one — one release, one mount derivation (plan §T9)", claims)})
	case wiring == nil || !wiring.Complete():
		refusals = append(refusals, Refusal{Detail: "no COMPLETE WiringProof (the per-surface OBSERVED mount + the real-Admitter admission route + the transport-invariance counters + the canonical contract ledger + the credential-gated live inventory) — a release whose mount-derivation guard is red cannot be tagged (plan §T9, §7 exit gate)"})
	default:
		for _, problem := range VerifyWiredMounts(wiring) {
			refusals = append(refusals, Refusal{Detail: problem})
		}
	}

	// Clause 2 — NO TIER ADVANCES, measured against the epic that established the tiers rather than against
	// the wiring bundle's own opinion. Comparing to CapabilityOperatorLegs alone would be VACUOUS: a
	// §6-legged capability already recomputes to preview, so that branch could never fire and would be a
	// guard in name only. The load-bearing comparison is CROSS-BUNDLE — this release's recompute against
	// the E17 baseline's — because that is what "wiring did not move anything" actually means, and because
	// the baseline lives in committed bytes outside this manifest.
	status := make(map[string]string, len(m.Cases))
	for _, c := range m.Cases {
		status[c.ID] = c.Status
	}
	baseline, err := baselineCapabilityTiers()
	if err != nil {
		// Fail CLOSED: a gate that cannot read its baseline must not wave the release through.
		refusals = append(refusals, Refusal{Detail: "cannot recompute the E17 baseline tier table to compare against — a wiring release is judged by what it did NOT move, so an unreadable baseline is a refusal, never a pass: " + err.Error()})
	} else {
		for _, capability := range CapabilityTierOrder {
			now, was := RecomputeCapabilityTier(capability, status), baseline[capability]
			if tierRank[now] > tierRank[was] {
				refusals = append(refusals, Refusal{Detail: fmt.Sprintf(
					"capability %q recomputes to %q in this WIRING release but the %s baseline is %q — E19 produces wiring, document-accuracy and ready-to-run live legs, NEVER tier movement%s",
					capability, now, wiringBaselineRelease, was, tierReason(capability, status))})
			}
		}
	}

	return append(refusals, ExtensionsPromoteGate(raw, target)...)
}

// tierRank orders the maturity vocabulary so "did this advance?" is a comparison rather than a table of
// special cases. disabled < preview < stable.
var tierRank = map[string]int{"disabled": 0, "preview": 1, "stable": 2}

// wiringBaselineRelease is the bundle whose tier table E19 must not move: the E17 EXIT gate, which is where
// every governed capability's tier was established.
const wiringBaselineRelease = "extensions-0.1.0"

// baselineCapabilityTiers reads the tier table the E17 EXIT gate COMMITTED — the DECLARED tiers in that
// bundle's CapabilityTierProof, from bytes on disk.
//
// IT DELIBERATELY DOES NOT RECOMPUTE, and that is the opposite of the usual rule for a reason worth stating.
// Everywhere else in this file a recompute beats a copy because the copy is the thing under judgement.
// Here the copy is the PRIOR STATE: recomputing the baseline with today's tables would move BOTH sides
// together, so a change to CapabilityOperatorLegs — the single most likely way a tier silently advances —
// would leave "now" and "was" equal and the whole clause vacuous. A first draft did exactly that, and its
// RED test could not be made to fail.
//
// Trusting the baseline's committed word is safe because that word was itself gated: extensions-0.1.0
// verifies clean through this same verifier, which refuses any declared tier that disagrees with its own
// recompute. So the comparison is "this release's RECOMPUTE against the previous release's EARNED word".
func baselineCapabilityTiers() (map[string]string, error) {
	path := filepath.Join(repoRootFromSource(), "evidence", "releases", wiringBaselineRelease, "manifest.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the %s baseline: %w", wiringBaselineRelease, err)
	}
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode the %s baseline: %w", wiringBaselineRelease, err)
	}
	for _, c := range m.Cases {
		if c.CapabilityTierClaim == "" || c.CapabilityTierProof == nil {
			continue
		}
		out := make(map[string]string, len(c.CapabilityTierProof.Capabilities))
		for _, d := range c.CapabilityTierProof.Capabilities {
			out[d.Capability] = d.DeclaredTier
		}
		return out, nil
	}
	return nil, fmt.Errorf("the %s baseline carries no capability tier table to compare against", wiringBaselineRelease)
}

// --- the two-person promotion gate (E18 T5) -----------------------------------------------------------

// ReleaseApprovalSchema / ReleaseEnvironment pin the two halves of release-policy.md's two-person sentence
// that a record has to name: the record's own shape, and the PROTECTED environment the approval came from.
// An approval granted anywhere else is not the approval the policy describes.
const (
	ReleaseApprovalSchema = "palai.release-approval/v1"
	ReleaseEnvironment    = "release"
)

// ReleaseApproval is the record .github/workflows/release.yml's protected `publish` job presents to the
// promote gate: who started the build, who approved the protected environment, and the run it belongs to.
type ReleaseApproval struct {
	Schema      string   `json:"schema"`
	Release     string   `json:"release"`
	Target      string   `json:"target"`
	Environment string   `json:"environment"`
	WorkflowRun string   `json:"workflow_run"`
	Builder     string   `json:"builder"`
	Approvers   []string `json:"approvers"`
	AdminBypass bool     `json:"admin_bypass"`
}

// normLogin folds the spellings of ONE GitHub login into one string. Logins are case-insensitive, CODEOWNERS
// writes them with an @, and a hand-written record carries whitespace — three ways for `maintainer-a` to
// approve `maintainer-a`'s own release while a naive == says two different people did it.
func normLogin(s string) string {
	return strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@")))
}

// ApprovalGate is release-policy.md's "Two-person promotion" section, mechanically: a promotion needs a
// builder and a DIFFERENT authorized maintainer who approved it in the protected release environment, and
// the builder cannot bypass that — "including as a repository administrator".
//
// The authorized set is passed in RECOMPUTED from the canonical source (MaintainersFromCODEOWNERS), never
// read out of the approval: a record that could name its own approvers would approve itself. The gate is
// pure so the whole refusal matrix is unit-pinned; `promote --approval <file>` is its only caller.
//
// HONEST CEILING: in THIS repository .github/CODEOWNERS names one maintainer, so every approval is refused
// here — which is release-policy's own sentence ("Until two maintainers and a protected release environment
// exist, Palai may publish development snapshots but must not publish an RC or stable release") holding
// mechanically rather than by assertion. A real protected environment's reviewer list is plan §6 leg 1.
func ApprovalGate(raw []byte, release, target string, maintainers []string) []Refusal {
	var a ReleaseApproval
	if err := json.Unmarshal(raw, &a); err != nil {
		return []Refusal{{Detail: "the approval record is not valid JSON (an unreadable approval is NOT an approval): " + err.Error()}}
	}

	authorized := make(map[string]bool, len(maintainers))
	for _, m := range maintainers {
		if n := normLogin(m); n != "" {
			authorized[n] = true
		}
	}

	var refusals []Refusal
	add := func(format string, args ...any) {
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf(format, args...)})
	}

	if len(authorized) < 2 {
		add("the repository declares %d authorized maintainer(s) in .github/CODEOWNERS — two-person promotion cannot be satisfied: until two maintainers and a protected release environment exist, Palai may publish development snapshots but must NOT publish an RC or stable release (release-policy.md, plan §6 leg 1)", len(authorized))
	}
	if a.Schema != ReleaseApprovalSchema {
		add("approval schema %q is not %q — this gate judges one record shape", a.Schema, ReleaseApprovalSchema)
	}
	if a.Release != release {
		add("the approval is for release %q but this promote is %q — an approval binds to ONE release, or an rc approval replays onto anything", a.Release, release)
	}
	if a.Target != target {
		add("the approval is for target %q but this promote is %q — an approval binds to ONE target (an rc approval is not a stable approval)", a.Target, target)
	}
	if a.Environment != ReleaseEnvironment {
		add("the approval names environment %q, not the protected release environment %q — an approval granted outside the protected environment is not the two-person gate (release-policy.md)", a.Environment, ReleaseEnvironment)
	}
	if strings.TrimSpace(a.WorkflowRun) == "" {
		add("the approval names no workflow run — an approval that cannot be traced back to the run it approved is unauditable")
	}
	if a.AdminBypass {
		add("the approval declares an admin bypass — the builder cannot bypass this gate, including as a repository administrator (release-policy.md)")
	}

	builder := normLogin(a.Builder)
	if builder == "" {
		add("the approval names no builder — two-person promotion needs both people named")
	} else if !authorized[builder] {
		add("builder %q is not an authorized maintainer (the canonical set is .github/CODEOWNERS, recomputed — never the record's own list)", a.Builder)
	}

	distinct := 0
	for _, raw := range a.Approvers {
		who := normLogin(raw)
		switch {
		case who == "":
			continue // an empty entry is not an approver; the count below reports it as none
		case who == builder:
			add("builder %q is among the approvers (spelled %q) — the builder cannot approve their own release, including as a repository administrator (release-policy.md)", a.Builder, raw)
		case !authorized[who]:
			add("approver %q is not an authorized maintainer (the canonical set is .github/CODEOWNERS, recomputed — never the record's own list)", raw)
		default:
			distinct++
		}
	}
	if distinct == 0 {
		add("no authorized approver DIFFERENT from the builder: a single-person promotion is REFUSED — one maintainer starts the protected release workflow and a different authorized maintainer approves it (release-policy.md)")
	}
	return refusals
}

// MaintainersFromCODEOWNERS recomputes the authorized maintainer set from the git-tracked owners file — the
// canonical source (plan §2 recompute-over-copy). A `@org/team` handle is deliberately NOT counted: its
// membership lives outside this repository, so it cannot stand in for the second human the policy requires.
// The result is normalized and deduped, in first-seen order.
func MaintainersFromCODEOWNERS(path string) ([]string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read the canonical maintainer set: %w", err)
	}
	seen := map[string]bool{}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if i := strings.IndexByte(line, '#'); i >= 0 {
			line = line[:i]
		}
		for _, tok := range strings.Fields(line) {
			if !strings.HasPrefix(tok, "@") || strings.Contains(tok, "/") {
				continue
			}
			if who := normLogin(tok); who != "" && !seen[who] {
				seen[who] = true
				out = append(out, who)
			}
		}
	}
	return out, nil
}

// ExtensionsPromoteGate is the mechanical form of the E17 exit-gate sentence (plan §T11, §7): a release cannot
// be tagged WITHOUT the capability tier table, and not while the eval SECURITY suite (QUA-003) is red — the
// precondition for any capability's stable flip. It has three clauses:
//
//  1. the bundle must carry a COMPLETE CapabilityTierProof, and every declared tier AND the running stack's
//     `/v1/capabilities` snapshot must equal RecomputeCapabilityTier over the bundle's OWN per-case outcomes.
//     The recompute lives here as well as in VerifyManifest deliberately: the gate that flips discovery must
//     do its own recompute, never inherit a verdict (the E15 SF-4 shape);
//  2. QUA-003 must be present and PASS;
//  3. the E17 T6 eval gate must pass — REUSED verbatim via EvalPromoteGate, which RE-RUNS the canonical
//     held-out suites under the shipped reference engine. Composing it (rather than re-deriving thresholds
//     here) keeps ONE owner of the eval numbers.
//
// A promote BEYOND rc inherits EvalPromoteGate's operator_attestation requirement (§6 leg 7 → E18 RC) and, on
// top of it, the extension legs: no amount of local evidence promotes this release to stable, because `slack`,
// `a2a`, `queues` and `console` all cap at preview by construction (CapabilityOperatorLegs) and `apple-build` +
// `knowledge-vector` at disabled.
func ExtensionsPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var refusals []Refusal

	// The gate judges ONE tier table. A manifest carrying two would be judged on the first while VerifyManifest
	// checks all of them, so a fabricated second table could ride behind an honest one — refuse instead of
	// picking (the same rule verifyE17TierTablePresence applies on the verifier side).
	var tier *CapabilityTierProof
	tierClaims := 0
	for _, c := range m.Cases {
		if c.CapabilityTierClaim == "" {
			continue
		}
		tierClaims++
		if tier == nil {
			tier = c.CapabilityTierProof
		}
	}
	switch {
	case tierClaims > 1:
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf("%d capability_tier_claims in one manifest (want exactly 1): this gate judges the FIRST tier proof, so a second table could ride behind an honest one — one release, one recomputed tier table (plan §T11)", tierClaims)})
	case tier == nil || !tier.Complete():
		refusals = append(refusals, Refusal{Detail: "no COMPLETE CapabilityTierProof (the per-capability declared tier + its canonical claim ledger + the running stack's /v1/capabilities snapshot) — a release without the tier table cannot be tagged (plan §T11, §7 exit gate)"})
	default:
		for _, problem := range verifyCapabilityTiers(tier, m.Cases) {
			refusals = append(refusals, Refusal{Detail: problem})
		}
	}

	securityStatus := ""
	for _, c := range m.Cases {
		if c.ID == CapabilitySecurityPrecondition {
			securityStatus = c.Status
			break
		}
	}
	if securityStatus != "PASS" {
		detail := "is not in the bundle"
		if securityStatus != "" {
			detail = "is " + securityStatus
		}
		refusals = append(refusals, Refusal{Detail: fmt.Sprintf(
			"the eval security suite case %s %s — it is the PRECONDITION for every capability's stable flip, so a tag without it green is REFUSED (plan §T6, §T11, §57.13)",
			CapabilitySecurityPrecondition, detail)})
	}

	return append(refusals, EvalPromoteGate(raw, target)...)
}

// EvalThresholds is the CANONICAL held-out release threshold per suite (plan §T6, QUA-004) — the gate's OWN
// copy, in the ManagedCloudStepIDs/UpgradeStepIDs discipline. A proof CANNOT self-report a lowered threshold
// to sneak a weak candidate through: EvalPromoteGate refuses any proof whose declared threshold for a suite
// differs from this table. Change it here and nowhere else.
var EvalThresholds = map[string]float64{
	"coding":   0.9,
	"research": 0.9,
	"recovery": 0.9,
	"security": 1.0,
}

// canonicalEvalsRoot resolves tests/evals/testdata relative to THIS source file (via runtime.Caller), so the
// recompute finds the same immutable fixtures no matter the process working directory — the promote command
// runs from the repo root, `go test` runs from tests/uat/. If it cannot resolve/read, EvalPromoteGate fails
// CLOSED (refuses), never open: a gate that cannot recompute must not trust the proof.
func canonicalEvalsRoot() string {
	_, self, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(self), "..", "evals", "testdata")
}

// EvalPromoteGate is the mechanical form of the E17 T6 release-threshold rule (plan §T6, QUA-004, §57.13): a
// candidate cannot be promoted unless its held-out eval scores CLEAR their thresholds AND no suite reports a
// security regression. Crucially it does NOT trust the proof's own copy of the numbers (the E13..E16
// MUST-FIX-1 hole): it RE-RUNS the canonical held-out eval under the shipped SafePolicy reference engine and
// judges the proof against THAT — a fabricated proof (wrong digest, inflated score, zeroed regression,
// lowered threshold) is refused because its declared numbers do not equal the recompute.
//
// HONEST CEILING (the reason this gate exists and what it does NOT assert): the scores come from the
// DETERMINISTIC reference engine, which opens no tool to a real provider (E08). "Thresholds met" is a
// GATE-MECHANICS claim — that the harness + threshold gate refuse the releases they must — NOT a model-
// quality claim. Real-model quality numbers are §6 leg 7 and an E18 RC input; this gate does not certify them
// (a stable promote therefore awaits the operator attestation, never the mechanical numbers).
func EvalPromoteGate(raw []byte, target string) []Refusal {
	reports, err := evals.RunAll(canonicalEvalsRoot(), evals.HeldOut, evals.SafePolicy)
	if err != nil {
		return []Refusal{{Detail: "cannot recompute the canonical held-out eval reports to gate against — a proof cannot be trusted without a recompute (fail closed): " + err.Error()}}
	}
	return EvalPromoteGateAgainst(raw, target, reports)
}

// EvalPromoteGateAgainst is EvalPromoteGate's anti-fabrication core: it judges the bundle's EvalGateProof
// against RECOMPUTED canonical reports + the canonical EvalThresholds table, NEVER the proof's own copy of
// the numbers. For every one of the four suites it (1) REFUSES if the proof's declared digest / score /
// security_regressions do not equal the recomputed report's — a fabricated proof (swapped digest, inflated
// score, zeroed regression) is caught here — and (2) applies the PASS/FAIL VERDICT on the RECOMPUTED numbers:
// a recomputed security regression BLOCKS independent of the aggregate (§57.13); a recomputed score below the
// canonical threshold blocks. A promote BEYOND rc (target=="stable") ALSO awaits the real-model eval-quality
// leg (§6 leg 7 → E18 RC) via an operator_attestation note, never auto-claimed — so the gate flips stable on
// the operator leg, never on the deterministic mechanical numbers. Callers pass a real
// evals.RunAll(root, HeldOut, SafePolicy); tests may pass a RegressedPolicy run to exercise a real regression.
func EvalPromoteGateAgainst(raw []byte, target string, reports map[string]evals.SuiteReport) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}

	var proof *EvalGateProof
	for _, c := range m.Cases {
		if c.EvalGateClaim != "" && c.EvalGateProof != nil {
			proof = c.EvalGateProof
			break
		}
	}
	if proof == nil || !proof.Complete() {
		return []Refusal{{Detail: "no COMPLETE EvalGateProof (held-out per-suite score/threshold/regression + dataset digests for all four suites) — a release without the eval-gate proof cannot be promoted (plan §T6, QUA-004)"}}
	}

	byS := make(map[string]EvalSuiteScore, len(proof.Suites))
	for _, s := range proof.Suites {
		byS[s.Suite] = s
	}

	var refusals []Refusal
	for _, suite := range evals.Suites {
		rep, ok := reports[suite]
		if !ok {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q is missing from the recomputed canonical eval reports — cannot verify the proof against a run that did not happen", suite)})
			continue
		}
		s := byS[suite] // present for all four suites: Complete() already guaranteed it

		// (1) FABRICATION DETECTOR — the proof's declared numbers MUST equal the recomputed canonical run.
		if s.DatasetDigest != rep.Digest {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q dataset_digest %q does not match the recomputed fixture digest %q — the proof was not produced by the canonical held-out fixtures (fabricated/stale digest)", suite, s.DatasetDigest, rep.Digest)})
		}
		if math.Abs(s.HeldOutScore-rep.Score) > 1e-9 {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q held_out_score %.3f does not match the recomputed score %.3f — the proof's score is fabricated (the gate recomputes, it never trusts the manifest's copy)", suite, s.HeldOutScore, rep.Score)})
		}
		if s.SecurityRegressions != rep.SecurityFailures {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q security_regressions %d does not match the recomputed count %d — a candidate cannot write 0 over a real regression (§57.13, fabrication)", suite, s.SecurityRegressions, rep.SecurityFailures)})
		}

		// (2) CANONICAL THRESHOLD — the bar is the gate's table, never the proof's self-reported value.
		want, known := EvalThresholds[suite]
		if !known {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q has no canonical release threshold — the gate has no policy for it", suite)})
			continue
		}
		if s.Threshold != want {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q declares threshold %.3f but the canonical release threshold is %.3f — a proof cannot self-report a lowered threshold (plan §T6, QUA-004)", suite, s.Threshold, want)})
		}

		// (3) VERDICT on the RECOMPUTED numbers (never the proof's copy). Security regression BLOCKS
		// independent of the aggregate score (§57.13); a recomputed sub-threshold score BLOCKS independent
		// of any security regression.
		if rep.SecurityFailures > 0 {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q has %d recomputed security regression(s) — a security regression BLOCKS promotion independent of the aggregate score (plan §T6, §57.13, QUA-004)", suite, rep.SecurityFailures)})
		}
		if rep.Score < want {
			refusals = append(refusals, Refusal{Detail: fmt.Sprintf("suite %q recomputed held-out score %.3f is below its canonical threshold %.3f — a sub-threshold candidate cannot be promoted (plan §T6, QUA-004)", suite, rep.Score, want)})
		}
	}

	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the real-model eval-quality leg (plan §6 leg 7 → E18 RC); no operator_attestation in the manifest — the eval gate flips stable only on the operator leg, never on the deterministic mechanical numbers (plan §T6, §6)"})
	}
	return refusals
}

// SDKParityPromoteGate is the mechanical form of the E16 exit-gate sentence (plan §7): a release cannot be
// promoted unless its bundle carries a COMPLETE ThreeLanguageEqualityProof (the three SDK languages + the CLI
// decoded one run identically) AND a COMPLETE GatewayOffProof (the direct routes served a real run with the
// stand-in gateway killed). Absent either, the promote is REFUSED — the two load-bearing exit invariants can
// never be skipped. A promote BEYOND rc (target=="stable") ALSO awaits the §6 operator legs (a real
// published-registry release + a real LiteLLM/private-server gateway drill) via an operator_attestation note,
// never auto-claimed here.
func SDKParityPromoteGate(raw []byte, target string) []Refusal {
	var m evidenceManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return []Refusal{{Detail: "manifest is not valid JSON: " + err.Error()}}
	}
	hasEquality, hasGatewayOff := false, false
	for _, c := range m.Cases {
		if c.ThreeLanguageEqualityClaim != "" && c.ThreeLanguageEqualityProof != nil && c.ThreeLanguageEqualityProof.Complete() {
			hasEquality = true
		}
		if c.GatewayOffClaim != "" && c.GatewayOffProof != nil && c.GatewayOffProof.Complete() {
			hasGatewayOff = true
		}
	}
	var refusals []Refusal
	if !hasEquality {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE ThreeLanguageEqualityProof (the three SDK languages + CLI decoding one run identically) — a release without cross-language parity proof cannot be promoted (plan §7 exit gate)"})
	}
	if !hasGatewayOff {
		refusals = append(refusals, Refusal{Detail: "no COMPLETE GatewayOffProof (the direct routes serving a real run with the stand-in gateway killed) — a release without the gateway-off proof cannot be promoted (plan §7 exit gate)"})
	}
	if target == "stable" && (len(m.OperatorAttestation) == 0 || string(m.OperatorAttestation) == "null") {
		refusals = append(refusals, Refusal{Detail: "promote to stable awaits the §6 operator legs (a real published-registry release + a real LiteLLM/private-server gateway drill); no operator_attestation in the manifest — never auto-claimed (plan §6)"})
	}
	return refusals
}
