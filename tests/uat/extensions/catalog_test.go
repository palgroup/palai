package extensions

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// extCase is the case.yaml catalog record for an E17 extension case — the same shape the automation /
// extensibility / managed-cloud gates decode, with a structured `proof:` list so this gate can assert the
// referenced proof genuinely exists in the tree.
//
// ponytail: the mciCase/validProofClasses/honestNamePattern/repoRoot quartet is the FIFTH copy-adaptation of
// tests/uat/automation/catalog_test.go. Hoisting it into a shared package is a separate refactor, not this task.
type extCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary an E17 case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// dockerBoundClasses are the proof classes whose //go:build TAG is load-bearing: they cannot run without a real
// backing service or a real credential, so a case declaring one MUST reference at least one proof carrying that
// exact tag. The untagged tiers (`unit`) and the httptest-server tier (`e2e-deterministic`) both ride
// tag-less-or-e2e files, so for those the gate asserts resolution rather than a tag — an A2A conformance suite
// driven over a real httptest server with a fake provider is genuinely e2e-deterministic whether or not anyone
// put an `e2e` tag on it. This is the honest mechanical rule: overclaiming a service/credential you never
// touched fails; the tag-irrelevant tiers are not force-relabelled.
var dockerBoundClasses = map[string]bool{"component-real": true, "live-provider": true, "fault-live": true}

// extensionIDPrefixes are the case-id families the E17 exit gate OWNS exclusively, so a stray dir under one of
// them cannot escape proof resolution. SUB- is deliberately absent (SUB-006 belongs to the E08 coding slice);
// SUB-007 is named in the catalog map explicitly instead. AUT- is absent for the same reason — AUT-009/010/013
// are pre-E17 cases that GAINED a queue/orchestrator leg (§T7/§T8) and are listed explicitly.
var extensionIDPrefixes = []string{"SLK-", "A2A-", "KNO-", "QUA-", "UI-", "WRK-"}

// expectedExtensionsCatalog is the E17 UAT catalog: every case this epic materializes (plan §T11 + §7) mapped
// to the proof class its case.yaml must declare and the in-tree proof(s) that prove it. A missing dir, a drifted
// class, a changed proof list, or a proof reference that does not resolve fails the gate.
//
// The three AUT- entries are NOT new ids: T7 and T8 appended a queue-adapter / orchestrator-kit proof leg to the
// EXISTING automation cases (plan §T7/§T8 — "leg EKLER, yeni ID açmaz"), and this gate pins those legs so they
// cannot silently disappear. UI-001/002's proofs are playwright specs, referenced as `file.spec.ts:<test title>`.
var expectedExtensionsCatalog = map[string]struct {
	class  string
	proofs []string
}{
	"SLK-001": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventNormalizesToCanonicalIdentity",
		"adapters/integrations/slack/inbound_test.go:TestUnwrapSocketFrameFeedsTheSameMapping",
	}},
	"SLK-002": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventRedeliveryIsIdentityStable",
		"apps/control-plane/internal/automation/dedupe_component_test.go:TestDuplicateDeliveryLinksOriginalSingleAction",
	}},
	"SLK-003": {"component-real", []string{
		"apps/control-plane/internal/extensions/slack_component_test.go:TestSlackThreadSessionCorrelation",
	}},
	"SLK-004": {"unit", []string{
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalBindsHashUserWorkspace",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalRejectsEverythingElse",
		"apps/control-plane/internal/extensions/slack_component_test.go:TestSlackConnectionCreateReadRLS",
	}},
	"SLK-005": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventClassifiesEditsAndDeletes",
	}},
	"SLK-006": {"unit", []string{
		"adapters/integrations/slack/ratelimit_test.go:TestPostMessageRepairsA429Once",
		"adapters/integrations/slack/ratelimit_test.go:TestPostMessageBoundedRepairThenRateLimited",
	}},
	"SLK-007": {"unit", []string{
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalBindsHashUserWorkspace",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalDenyIsMapped",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalRejectsEverythingElse",
	}},
	"SLK-008": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsBotAndSelfEvents",
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsNestedBotEdit",
	}},
	"A2A-001": {"component-real", []string{
		"adapters/integrations/a2a/card_test.go:TestAgentCardNeverLeaksInternalDetail",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_PublicCardNeverLeaksAndAdvertisesExactVersion",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_ExtendedCardRequiresAuth",
		"tests/component/postgres/a2a_store_test.go:TestA2AStoreRLSAndCanonicalRefInvariant",
	}},
	"A2A-002": {"e2e-deterministic", []string{
		"adapters/integrations/a2a/card_test.go:TestGovernIdentityIgnoresForgedMetadata",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_SendReturnsTaskWithExternalCanonicalSeparation",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_ForgedMetadataIdentityIsIgnored",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_SendReturnsDirectMessageForCompleteNonDurable",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_InboundFilePartIsIngested",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_StreamTerminalConsistency",
		"adapters/integrations/a2a/conformance_test.go:TestA2ALoopbackExchange",
		"tests/component/postgres/a2a_store_test.go:TestA2AStoreRLSAndCanonicalRefInvariant",
	}},
	"A2A-003": {"e2e-deterministic", []string{
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_InputRequiredMapsToWaiting",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_TaskGetListCancel",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_PushConfigCRUD",
	}},
	"A2A-004": {"e2e-deterministic", []string{
		"adapters/integrations/a2a/client_test.go:TestA2AClientRefusesInternalCardEndpoint",
		"adapters/integrations/a2a/client_test.go:TestA2AClientRevalidatesRedirectToInternal",
		"adapters/integrations/a2a/client_test.go:TestA2AClientRefusesDisallowedExtensionURI",
		"adapters/integrations/a2a/client_test.go:TestA2AClientVersionNegotiationExplicitFail",
		"adapters/integrations/a2a/client_test.go:TestA2AClientIngestsAndScansPushedFile",
		"adapters/integrations/a2a/conformance_test.go:TestA2AConformance_InboundFilePartIsIngested",
		"packages/egress/egress_test.go:TestVetIPDeniesMetadataPrivateAndSpecialUse",
	}},
	"A2A-005": {"e2e-deterministic", []string{
		"adapters/integrations/a2a/client_test.go:TestA2AClientRemoteOutputIsUntrusted",
		"adapters/integrations/a2a/client_test.go:TestA2AClientNeverInheritsParentCredential",
		"adapters/integrations/a2a/client_test.go:TestA2AClientNeedsResolverForAuthConnection",
		"adapters/integrations/a2a/client_test.go:TestA2AClientRemoteTaskIDsAreConnectionScoped",
		"adapters/integrations/a2a/client_test.go:TestA2AClientLoopbackInteropAgainstT2Server",
		"tests/component/postgres/a2a_remote_agent_store_test.go:TestA2ARemoteAgentStoreRLS",
	}},
	"SUB-007": {"e2e-deterministic", []string{
		"adapters/integrations/a2a/client_test.go:TestA2AClientRemoteChildIsUntrustedAndNoCredentialInheritance",
		"adapters/integrations/a2a/client_test.go:TestA2AClientLoopbackInteropAgainstT2Server",
	}},
	"KNO-001": {"component-real", []string{
		"apps/control-plane/internal/knowledge/knowledge_component_test.go:TestIngestIsImmutableAndVersioned",
		"apps/control-plane/internal/knowledge/knowledge_component_test.go:TestFTSRanksAndCitesWithVerifiableOffsets",
		"apps/control-plane/internal/knowledge/chunk_test.go:TestChunkDocumentIsDeterministicWithVerifiableOffsets",
	}},
	"KNO-002": {"component-real", []string{
		"apps/control-plane/internal/knowledge/knowledge_component_test.go:TestFailedRefreshLeavesPriorActiveIntact",
	}},
	"KNO-003": {"component-real", []string{
		"apps/control-plane/internal/knowledge/retrieval_acl_component_test.go:TestForgedACLGrantInBodyIsRejectedAndGovernedByScope",
		"apps/control-plane/internal/knowledge/retrieval_acl_component_test.go:TestPostFilterTopKIsForbidden",
		"apps/control-plane/internal/knowledge/retrieval_acl_component_test.go:TestCrossProjectACLNegative",
		"apps/control-plane/internal/knowledge/knowledge_component_test.go:TestRetrievalIsTenantAndACLScoped",
		"apps/control-plane/internal/knowledge/retrieval_unit_test.go:TestDerivePrincipalGrantsFromKeyScopesOnly",
	}},
	"KNO-004": {"component-real", []string{
		"apps/control-plane/internal/knowledge/knowledge_component_test.go:TestSourceDeletePropagates",
	}},
	"KNO-005": {"component-real", []string{
		"apps/control-plane/internal/knowledge/retrieval_strategy_component_test.go:TestHybridStrategyFusesKeywordAndVectorWithCitations",
		"apps/control-plane/internal/knowledge/retrieval_strategy_component_test.go:TestVectorStrategyDisabledByDefault",
		"apps/control-plane/internal/knowledge/retrieval_unit_test.go:TestResolveVectorHitsDropsLeakyRecords",
		"apps/control-plane/internal/knowledge/retrieval_unit_test.go:TestReciprocalRankFusionRewardsBothStrategies",
	}},
	"KNO-006": {"e2e-deterministic", []string{
		"apps/control-plane/internal/execution/tools/knowledge_test.go:TestRetrievalToolQuarantinesUntrustedContent",
		"apps/control-plane/internal/execution/tools/knowledge_test.go:TestRetrievalToolCitesWithOffsets",
		"apps/control-plane/internal/knowledge/retrieval_tool_component_test.go:TestRetrievalToolCitesVerifiableOffsetsEndToEnd",
	}},
	"KNO-007": {"component-real", []string{
		"apps/control-plane/internal/knowledge/retrieval_strategy_component_test.go:TestRestrictedSourceNotEmbeddedToDisallowedRegion",
		"apps/control-plane/internal/knowledge/retrieval_unit_test.go:TestEmbeddingPolicyBlocksRestrictedToDisallowedTarget",
	}},
	"KNO-008": {"component-real", []string{
		"apps/control-plane/internal/knowledge/retrieval_strategy_component_test.go:TestFreshnessDeadlineFailsWarnsNeverSilentStale",
		"apps/control-plane/internal/knowledge/retrieval_unit_test.go:TestEvaluateFreshness",
	}},
	"QUA-001": {"unit", []string{
		"tests/evals/harness_test.go:TestFourSuitesGreen",
		"tests/evals/harness_test.go:TestDigestIsContentAddressed",
		"tests/evals/integration_test.go:TestIntegrationBenchmark",
	}},
	"QUA-002": {"unit", []string{
		"tests/evals/harness_test.go:TestModelJudgeNeverSoleGateForProtected",
		"tests/evals/harness_test.go:TestRegressedPolicyIsDetectable",
		"tests/evals/live_test.go:TestModelJudgeCalibrationSmoke",
	}},
	"QUA-003": {"unit", []string{
		"tests/evals/harness_test.go:TestFourSuitesGreen",
		"tests/evals/harness_test.go:TestRegressedPolicyIsDetectable",
		"tests/uat/eval_gate_test.go:TestEvalPromoteGateBlocksRealSecurityRegression",
	}},
	"QUA-004": {"unit", []string{
		"tests/uat/eval_gate_test.go:TestEvalPromoteGateRefusesSubThreshold",
		"tests/uat/eval_gate_test.go:TestEvalPromoteGateBlocksRealSecurityRegression",
		"tests/uat/eval_gate_test.go:TestEvalPromoteGateRefusesMissingProof",
	}},
	"UI-001": {"e2e-deterministic", []string{
		"apps/web-console/tests/a11y.spec.ts:axe-core reports zero violations on the admin surface",
		"apps/web-console/tests/a11y.spec.ts:axe-core reports zero violations on the live-run surface after a completed run",
		"apps/web-console/tests/a11y.spec.ts:keyboard navigation: skip link is the first stop and the run→approve flow works with no mouse",
	}},
	"UI-002": {"e2e-deterministic", []string{
		"apps/web-console/tests/journey.spec.ts:UI-002: the approval UI shows the authoritative operation/branch/request_hash from the canonical event — the proposal display string does not replace them",
		"apps/web-console/tests/journey.spec.ts:approve proceeds through recovery to a completed run with a downloadable artifact",
		"apps/web-console/tests/journey.spec.ts:deny blocks the operation — the run terminates canceled, the push never completes",
	}},
	"WRK-001": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerEnrollTypedDispatchAndArtifactRoundTrip",
		"apps/control-plane/internal/workers/catalog_test.go:TestAppleBuildCapabilityIsAbsentEverywhere",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestTwoSidedTypedJobRoundTrip",
	}},
	"WRK-002": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerEnrollTypedDispatchAndArtifactRoundTrip",
		"apps/control-plane/internal/workers/catalog_test.go:TestNoTunnel_TypedOperationPassesTheGate",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestTwoSidedTypedJobRoundTrip",
	}},
	"WRK-003": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerFenceStaleRejectOnRedispatch",
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerHealthChangeCutsLease",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestFenceStaleRejectThroughGateway",
	}},
	"WRK-004": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestSecretHandleScopeAndExpiry",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestTwoSidedTypedJobRoundTrip",
	}},
	"WRK-005": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerEnrollTypedDispatchAndArtifactRoundTrip",
		"apps/control-plane/internal/workers/buildcheck_test.go:TestBuildCheckIsHonestAndDeterministic",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestTwoSidedTypedJobRoundTrip",
	}},
	"WRK-006": {"unit", []string{
		"apps/control-plane/internal/workers/catalog_test.go:TestNoTunnel_UntypedOperationIsRefusedAtDispatch",
		"apps/control-plane/internal/workers/catalog_test.go:TestAppleBuildCapabilityIsAbsentEverywhere",
		"apps/control-plane/internal/workers/workers_component_test.go:TestNoTunnelSubmitForForeignOperationRefused",
		"apps/control-plane/internal/workers/live/worker_live_test.go:TestNoTunnelSurface",
	}},
	"WRK-007": {"component-real", []string{
		"apps/control-plane/internal/workers/workers_component_test.go:TestQuarantineOnUncertain",
		"apps/control-plane/internal/workers/workers_component_test.go:TestWorkerFenceStaleRejectOnRedispatch",
	}},
	"AUT-009": {"component-real", []string{
		"apps/control-plane/internal/automation/inbound_component_test.go:TestRedeliveryAfterLostAckDoesNotDuplicate",
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterRedeliversAfterLostAckSingleEffect",
	}},
	"AUT-010": {"component-real", []string{
		"apps/control-plane/internal/automation/inbound_component_test.go:TestFloodBoundsMemoryReportsDepthApplies429",
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterFloodAppliesBackpressureNoDrop",
	}},
	"AUT-013": {"component-real", []string{
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetrySameIdempotencyKeySingleEverything",
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetryDifferentKeySameDedupeSingleAction",
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterRedeliversAfterLostAckSingleEffect",
		"apps/control-plane/internal/automation/orchestrator_storm_component_test.go:TestOrchestratorRetryStormSingleRun",
	}},
}

// localSeamWords are the seam vocabulary plan §T11 requires every E17 case text to NAME: whether the local
// proof is a fake peer, a loopback exchange, a deterministic run, a live credentialed call, or a real backing
// service. A case that does not say which of these produced its evidence is the prose half of an overclaim.
var localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam"}

// TestExtensionsCatalogMaterialized is the E17 catalog gate (plan §T11): every case this epic owns is present,
// honestly named, declares a §10.2 class its referenced proofs support, points at proofs that actually exist in
// the tree, and — the §T11-specific rule — NAMES both its local seam and its §6 operator leg in the case text.
// It rides make verify (no Docker, no credential), so a forgotten case, a vanished proof leg, or a case text
// that hides which seam produced its evidence fails fast.
func TestExtensionsCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedExtensionsCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c extCase
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Errorf("%s: decode case.yaml: %v", id, err)
			continue
		}
		if c.ID != id {
			t.Errorf("%s: id = %q, want the directory name", id, c.ID)
		}
		if c.ProofClass != want.class {
			t.Errorf("%s: proof_class = %q, want %q", id, c.ProofClass, want.class)
		}
		if !validProofClasses[c.ProofClass] {
			t.Errorf("%s: proof_class = %q, not a master-plan §10.2 class", id, c.ProofClass)
		}
		if !honestNamePattern.MatchString(c.Name) {
			t.Errorf("%s: name = %q, want a kebab-case behaviour assertion", id, c.Name)
		}
		if c.Provider == "" || c.Input == "" || c.ExpectStatus == "" {
			t.Errorf("%s: provider/input/expect_status must all be set (case.yaml discipline)", id)
		}
		if c.ProofClass == "live-provider" && c.Provider == "fake" {
			t.Errorf("%s: a live-provider case must not declare the fake provider", id)
		}

		// Plan §T11: the case text names its LOCAL seam and its §6 operator leg. Without the seam a reader
		// cannot tell a fake-peer proof from a real one; without the leg the bundle silently implies the
		// external receipt exists.
		lower := strings.ToLower(c.Input)
		named := false
		for _, word := range localSeamWords {
			if strings.Contains(lower, word) {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — plan §T11 requires it", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — plan §T11 requires every E17 case to say what real-infrastructure leg it does NOT cover", id)
		}

		assertExtensionProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// Orphan guard: every case dir under an E17-exclusive prefix must be in the map, so a stray case cannot
	// escape proof resolution.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range extensionIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedExtensionsCatalog[e.Name()]; !ok {
					t.Errorf("%s: an E17-family case dir is not in expectedExtensionsCatalog (add it, or it escapes proof resolution)", e.Name())
				}
				break
			}
		}
	}
}

// TestEveryCapabilityClaimIsACatalogedCase closes the loop between the tier ledger and the catalog: every claim
// id the CapabilityClaims table computes a tier over must be a case this gate resolves proofs for, and the
// QUA-003 security precondition too. Otherwise a capability could be governed by a claim nobody proves.
func TestEveryCapabilityClaimIsACatalogedCase(t *testing.T) {
	for _, id := range allClaimCases() {
		if _, ok := expectedExtensionsCatalog[id]; !ok {
			t.Errorf("claim %q is in the capability tier ledger but not in expectedExtensionsCatalog — a tier cannot be computed over an unproven claim", id)
		}
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no build tag
// runs in make verify / test-unit, so it is the "unit" tier. A .ts file is a playwright spec: the
// e2e-deterministic tier (it is driven by the console's own runner, not `go test`).
func buildClass(path, body string) string {
	if strings.HasSuffix(path, ".ts") {
		return "e2e-deterministic"
	}
	for _, line := range strings.Split(body, "\n") {
		if constraint, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:build "); ok {
			switch strings.TrimSpace(constraint) {
			case "fault":
				return "fault-live"
			case "component":
				return "component-real"
			case "e2e":
				return "e2e-deterministic"
			case "live":
				return "live-provider"
			default:
				return "unit"
			}
		}
	}
	return "unit"
}

// assertExtensionProofs checks the case.yaml `proof:` list equals the catalog's expected list and that each
// reference RESOLVES: a Go "path/file.go:FuncName" must declare that func; a "path/file.spec.ts:<test title>"
// must contain that title. It then applies the tier rule — a case declaring a Docker/credential-bound class
// must reference at least one proof carrying that exact build tag, so a case can never claim a real backing
// service or a real provider it never touched.
func assertExtensionProofs(t *testing.T, root, id, class string, want, got []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: proof list = %v, want %v", id, got, want)
		return
	}
	tiers := map[string]bool{}
	for _, ref := range got {
		file, name, ok := strings.Cut(ref, ":")
		if !ok {
			t.Errorf("%s: proof %q is not file:reference", id, ref)
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Errorf("%s: proof file %q does not exist: %v", id, file, err)
			continue
		}
		body := string(raw)
		switch {
		case strings.HasSuffix(file, ".ts"):
			if !strings.Contains(body, name) {
				t.Errorf("%s: playwright test %q not found in %s (the case claims a spec that is not in the tree)", id, name, file)
			}
		default:
			if !strings.Contains(body, "func "+name+"(") {
				t.Errorf("%s: proof %q not found in %s (the case claims a proof that is not in the tree)", id, name, file)
			}
		}
		tiers[buildClass(file, body)] = true
	}
	if dockerBoundClasses[class] && !tiers[class] {
		t.Errorf("%s: declares proof_class %q (a Docker/credential-bound tier) but references no proof carrying the matching //go:build tag — tier overclaim; referenced tiers were %v", id, class, tiers)
	}
	if !dockerBoundClasses[class] && !tiers["unit"] && !tiers["e2e-deterministic"] {
		t.Errorf("%s: declares proof_class %q but every referenced proof is Docker/credential-bound (%v) — the declared tier cannot actually run this case's proof", id, class, tiers)
	}
}

// repoRoot walks up to the module root (the dir holding go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
