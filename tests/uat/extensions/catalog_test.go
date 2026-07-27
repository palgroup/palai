package extensions

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/tests/uat"
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
//
// TLM- (E21) joined the list even though NO TLM- case is in expectedExtensionsCatalog and none ever will be.
// That is the point: this sweep is the ONLY place in the tree that walks the cases DIRECTORY, so a prefix
// left outside it is a family whose dirs nothing checks — the quiet way of piercing a guard that still
// reports green. Ownership is allowed to live elsewhere (uat.AgentSurfaceCaseIDs, uat.ToolsMemoryCaseIDs);
// escaping the sweep is not.
var extensionIDPrefixes = []string{"SLK-", "A2A-", "KNO-", "QUA-", "TLM-", "UI-", "WRK-"}

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
	// E19 T3 moved SLK-001 from `unit` to `component-real` and it is an EARNED upgrade, not a relabel: the
	// transport-invariance claim is now asserted by the shipped Socket Mode connect loop driving the real
	// admission bridge against real Postgres, in both transport orders. The protocol half stays untagged on
	// purpose (a fake WSS server over httptest needs no service), so it runs under a plain `go test` rather
	// than hiding behind a Postgres skip.
	"SLK-001": {"component-real", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventNormalizesToCanonicalIdentity",
		"adapters/integrations/slack/inbound_test.go:TestUnwrapSocketFrameFeedsTheSameMapping",
		"adapters/integrations/slack/socket_test.go:TestSocketFrameCarriesAcceptsResponsePayload",
		"adapters/integrations/slack/socket_test.go:TestSocketDisconnectReasonIsDecoded",
		"adapters/integrations/slack/socket_test.go:TestSocketAckShape",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketAcknowledgesBeforeDoingTheWork",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketAckShapeMatchesTheDocumentedEnvelope",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketOverlapsOnWarningAndLosesNoEvents",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketStopsPermanentlyOnLinkDisabled",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketDrainsGracefully",
		"apps/control-plane/internal/extensions/slack_socket_test.go:TestSlackSocketNeverLogsTheTokenOrTheTicket",
		"apps/control-plane/internal/store/slack_socket_component_test.go:TestSlackSocketModeAndHTTPShareOneCanonicalIdentity",
	}},
	// E19 T1 shipped the Events API route, so the redelivery collapse is now asserted on the REAL route
	// against real Postgres rather than only on the pure mapping: unit -> component-real.
	"SLK-002": {"component-real", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventRedeliveryIsIdentityStable",
		"apps/control-plane/internal/automation/dedupe_component_test.go:TestDuplicateDeliveryLinksOriginalSingleAction",
		"apps/control-plane/internal/store/slack_events_component_test.go:TestSlackRetryStormRunsTheEffectOnce",
	}},
	// The post-E20 live fix EXTENDED this case rather than opening an id, and the reason is that it is the SAME
	// invariant read from the other side: the thread↔session correlation is what decides whether a thread's
	// earlier messages are read. A thread with a session is never read (run.start replays it); a REPLY in a
	// thread without one is read exactly once. Both halves are listed, because the negative is the kind of claim
	// that rots into a vacuous green — drop the "does not fetch twice" leg and the remaining one is satisfied by
	// a build that fetches on every single message.
	"SLK-003": {"component-real", []string{
		"adapters/integrations/slack/history_test.go:TestThreadRepliesCarriesTheDocumentedArguments",
		"adapters/integrations/slack/history_test.go:TestThreadRepliesRefusesAnUnboundedOrUnaddressedRead",
		"adapters/integrations/slack/history_test.go:TestThreadRepliesDropsSubtypedAndEmptyMessages",
		"adapters/integrations/slack/history_test.go:TestThreadRepliesTypesTheAPIRefusal",
		"adapters/integrations/slack/history_test.go:TestThreadRepliesDoesNotRetryARateLimit",
		"adapters/integrations/slack/inbound_test.go:TestMapEventCarriesTheAffectedMessageTS",
		"apps/control-plane/internal/extensions/slack_thread_test.go:TestSlackThreadNoteIsEmptyWhenThereIsNothingToQuote",
		"apps/control-plane/internal/extensions/slack_thread_test.go:TestSlackThreadNoteLeadsWithTheUntrustedLabelAndEndsBeforeTheRequest",
		"apps/control-plane/internal/extensions/slack_thread_test.go:TestSlackThreadNoteAttributesWithoutNamingAnyone",
		"apps/control-plane/internal/extensions/slack_thread_test.go:TestSlackThreadNoteMakesBothTruncationsVisible",
		"apps/control-plane/internal/extensions/slack_thread_test.go:TestSlackThreadNoteQuotesAnInjectionAsData",
		"apps/control-plane/internal/extensions/slack_component_test.go:TestSlackThreadSessionCorrelation",
		"apps/control-plane/internal/store/slack_events_component_test.go:TestSlackThreadCorrelatesToOneSession",
		"apps/control-plane/internal/store/slack_events_component_test.go:TestSlackConcurrentFirstEventsNeverSplitAThread",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackThreadHistoryIsFetchedOnceForAThreadWeWereInvitedInto",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackTopLevelMentionReadsNoThread",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackThreadReadAddressesTheEventNotTheContext",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackThreadHistoryIsNotReadOutsideTheChannelAllowList",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackThreadHistoryRefusalStillAdmitsTheRun",
		"apps/control-plane/internal/store/slack_thread_history_component_test.go:TestSlackThreadHistoryRedeliveryStillReplays",
	}},
	// E19 T2 gave ApproverAuthorized its FIRST production caller, which is what earns deleting E17 T11's
	// "UNWIRED DECISION PATH" note: the allow-list is now enforced on the shipped interactivity route.
	"SLK-004": {"component-real", []string{
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalBindsHashUserWorkspace",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalRejectsEverythingElse",
		"apps/control-plane/internal/extensions/slack_component_test.go:TestSlackConnectionCreateReadRLS",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackUnauthorizedClickEnqueuesNothing",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackStaleHashAndForeignThreadDecideNothing",
	}},
	// E20 extended this case rather than opening an id, and what it added is the half SLK-005 had DECLARED as
	// its own ceiling: the file leg. The fetch, the artifact, the input's image_ref, the provider's content
	// part, the caps and the visible refusals are all this case's claim now, so the ceiling sentence is gone
	// from case.yaml and the proof block carries the tests that replaced it.
	"SLK-005": {"component-real", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventClassifiesEditsAndDeletes",
		"adapters/integrations/slack/files_test.go:TestFetchImageUsesTheTokenOnlyAsABearerHeader",
		"adapters/integrations/slack/files_test.go:TestFetchImageRefusesAnyHostButSlackFiles",
		"adapters/integrations/slack/files_test.go:TestFetchImageRefusesBytesThatAreNotAnImage",
		"apps/control-plane/internal/execution/vision_test.go:TestDecodeMessagesResolvesAnImageRefToBytes",
		"apps/control-plane/internal/execution/vision_test.go:TestDecodeMessagesTakesNothingButBytesFromAnImageItem",
		"apps/control-plane/internal/execution/vision_component_test.go:TestDispatchResolvesAnImageRefIntoTheProviderRequest",
		"adapters/models/provider_one/vision_test.go:TestBuildBodyRendersAnImageAsAContentPart",
		"apps/control-plane/internal/store/slack_events_component_test.go:TestSlackEditsAndDeletesReachAdmissionAsTheirOwnKind",
		"apps/control-plane/internal/store/slack_image_component_test.go:TestSlackSharedImageBecomesAnImageRefInTheRunInput",
		"apps/control-plane/internal/store/slack_image_component_test.go:TestSlackImageRedeliveryReplaysOntoTheSameRunAndArtifact",
		"apps/control-plane/internal/store/slack_image_component_test.go:TestSlackNonImageAndOversizeFilesAreRefusedVisibly",
		"apps/control-plane/internal/artifacts/inbound_image_component_test.go:TestArtifactInboundImageWriteIsIdempotentAtACallerChosenID",
		"apps/control-plane/internal/artifacts/inbound_image_component_test.go:TestInboundImageIsReachedByRetentionOnlyOnceAttached",
	}},
	"SLK-006": {"component-real", []string{
		"adapters/integrations/slack/ratelimit_test.go:TestPostMessageRepairsA429Once",
		"adapters/integrations/slack/ratelimit_test.go:TestPostMessageBoundedRepairThenRateLimited",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackDecisionRepairsTheVisibleMessageExactlyOnceUnderA429",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackCoalescedUpdatesArePacedPerChannel",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackDecisionSurvivesAPermanentlyRateLimitedSlack",
		// E20 T1 extended this case rather than opening an id: the streaming calls ride the SAME retry owner
		// and the SAME pacer, so "one repair, no second layer" is now asserted on the stream cadence too.
		"adapters/integrations/slack/stream_test.go:TestStreamCallsTruncateTheirOwnText",
		"apps/control-plane/internal/store/slack_stream_component_test.go:TestSlackStreamAppendRepairsA429WithoutASecondRetryLayer",
	}},
	// E20 T4 extended this case rather than opening an id — the same move T1 made on SLK-006, and for the same
	// reason: the claim is already here. "ONLY from a MINTED button" always had a supply side nobody proved,
	// and the renderer is where it is now proven — a model's forged approve button falls to inert text, so the
	// "foreign button" this case refuses downstream never reaches a human upstream either. (SLK-012, the id
	// the T4 seam names, lands with T5: cataloging a NEW case regenerates extensions-0.1.0, and governing it
	// grows CapabilityClaims["slack"], which three committed manifests and the 1.0 RC recompute from.)
	"SLK-007": {"component-real", []string{
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalBindsHashUserWorkspace",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalDenyIsMapped",
		"adapters/integrations/slack/approval_test.go:TestMapInteractiveApprovalRejectsEverythingElse",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackAuthorizedClickApprovesThroughTheWholeChain",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackDenyClickDeniesThePublication",
		"adapters/integrations/slack/blocks_test.go:TestRenderRefusesToMintAnActionableElementFromModelOutput",
		"adapters/integrations/slack/blocks_test.go:TestApprovalMessageIsTheOnlyMintOfAnActionableElement",
		"adapters/integrations/slack/blocks_test.go:TestNoFileButInteractionsMintsAnActionableElement",
		"apps/control-plane/internal/store/slack_stream_component_test.go:TestSlackStopStreamCarriesRenderedBlocksAndNoForgedButton",
	}},
	// The self-loop guard lives in the pure MapEvent, so it holds on every transport — but "it should" is
	// not evidence, and a bot event that opened a run over a WebSocket would be a loop nobody notices.
	"SLK-008": {"component-real", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsBotAndSelfEvents",
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsNestedBotEdit",
		"apps/control-plane/internal/store/slack_events_component_test.go:TestSlackBotSelfEventBirthsNothing",
		"apps/control-plane/internal/store/slack_socket_component_test.go:TestSlackSocketModeSelfEventBirthsNothing",
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
		"adapters/integrations/a2a/conformance_test.go:TestA2APushCRUDIsUnmountedWithoutAPusher",
		"adapters/integrations/a2a/conformance_test.go:TestA2APushFailureDoesNotCorruptTheTask",
		"adapters/integrations/a2a/pusher_test.go:TestPushBodyIsAStreamResponseNotATask",
		"adapters/integrations/a2a/pusher_test.go:TestStreamResponseRequiresExactlyOneMember",
		"adapters/integrations/a2a/pusher_test.go:TestPushRefusesSSRFTargets",
		"adapters/integrations/a2a/pusher_test.go:TestPushAllowlistMatchesTheWholeHost",
		"adapters/integrations/a2a/pusher_test.go:TestPushRevalidatesRedirectsRatherThanDenyingThemAll",
		"adapters/integrations/a2a/pusher_test.go:TestPushCarriesTokenTimestampAndSingleUseID",
		"adapters/integrations/a2a/pusher_test.go:TestPushRetriesWithoutLossWhileTheSinkIsDown",
		"adapters/integrations/a2a/pusher_test.go:TestPushDeadLettersAfterTheAttemptBound",
		"adapters/integrations/a2a/pusher_test.go:TestPushTerminalRejectIsNotRetried",
		"apps/control-plane/api/a2a_wiring_test.go:TestA2APushSurfaceStaysUnmountedWithoutAConfiguredPusher",
		"apps/control-plane/api/a2a_wiring_test.go:TestA2APushMountsOnlyWithAConfiguredPusher",
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
	// E19 T5 wired RemoteChildRun into orchestrator.dispatchChild, so this case's proof_class moves
	// e2e-deterministic -> component-real: the crown is now asserted by the SHIPPED dispatch path over real
	// Postgres, not by the client alone.
	"SUB-007": {"component-real", []string{
		"adapters/integrations/a2a/client_test.go:TestA2AClientRemoteChildIsUntrustedAndNoCredentialInheritance",
		"adapters/integrations/a2a/client_test.go:TestA2AClientLoopbackInteropAgainstT2Server",
		"adapters/integrations/a2a/client_test.go:TestA2AClientIngestsAPushedFileOnTheDIRECTMESSAGEBranchToo",
		"apps/control-plane/internal/execution/remote_child_component_test.go:TestRemoteChildDispatchesToTheRegisteredRemote",
		"apps/control-plane/internal/execution/remote_child_component_test.go:TestRemoteChildNeverInheritsTheParentCredential",
		"apps/control-plane/internal/execution/remote_child_component_test.go:TestRemoteChildFailureIsAnHonestParentTerminal",
		"apps/control-plane/internal/execution/remote_child_component_test.go:TestRemoteChildIsRefusedRatherThanRunLocally",
		"apps/control-plane/internal/execution/child_dispatch_test.go:TestRemoteAgentRidesTheChildRequestOnlyWhenNamed",
		"apps/control-plane/internal/execution/child_dispatch_test.go:TestRemoteChildrenCountAgainstFanout",
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
	// E19 T7 gave both UI cases a REAL-upstream leg (no new id — the E17 T7 precedent of appending a leg to an
	// existing case). The a11y/journey specs now run against a compose control plane on the same spec files,
	// and the conformance sweep entries are the D15 crown: fixture drift against the real router is caught by
	// a test rather than by a reviewer. Pinning the sweep here stops those legs from silently disappearing.
	"UI-001": {"e2e-deterministic", []string{
		"apps/web-console/tests/a11y.spec.ts:axe-core reports zero violations on the admin surface",
		"apps/web-console/tests/a11y.spec.ts:axe-core reports zero violations on the live-run surface after a completed run",
		"apps/web-console/tests/a11y.spec.ts:keyboard navigation: skip link is the first stop and the run→approve flow works with no mouse",
		"apps/web-console/tests/conformance.test.mjs:every (method, path-pattern) the fixture serves is registered by the RUNNING real router",
	}},
	"UI-002": {"e2e-deterministic", []string{
		"apps/web-console/tests/journey.spec.ts:UI-002: the approval UI shows the authoritative operation/branch/request_hash from the canonical event — the proposal display string does not replace them",
		"apps/web-console/tests/journey.spec.ts:approve proceeds through recovery to a completed run with a downloadable artifact",
		"apps/web-console/tests/journey.spec.ts:deny blocks the operation — the run terminates canceled, the push never completes",
		"apps/web-console/tests/journey.spec.ts:the console renders a run's terminal state end to end — model, status and output from the upstream",
		"apps/web-console/tests/conformance.test.mjs:the fixture's scripted event vocabulary and payload keys match a REAL run's journal",
		"apps/web-console/tests/conformance.test.mjs:no ledger row has gone stale — every row was re-observed against the running real stack",
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
		// WIRED leg (E19 T6): the redelivery admits ONE RUN through the shipped bridge + real admission.
		"apps/control-plane/internal/automation/queue_bridge_component_test.go:TestQueueBridgeLostAckRedeliversToExactlyOneRun",
	}},
	"AUT-010": {"component-real", []string{
		"apps/control-plane/internal/automation/inbound_component_test.go:TestFloodBoundsMemoryReportsDepthApplies429",
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterFloodAppliesBackpressureNoDrop",
		// WIRED leg (E19 T6): the flood is shed and drained through the shipped bridge, one run per message.
		"apps/control-plane/internal/automation/queue_bridge_component_test.go:TestQueueBridgeFloodBackpressureNoDropOneRunEach",
	}},
	"AUT-013": {"component-real", []string{
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetrySameIdempotencyKeySingleEverything",
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetryDifferentKeySameDedupeSingleAction",
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterRedeliversAfterLostAckSingleEffect",
		"apps/control-plane/internal/automation/orchestrator_storm_component_test.go:TestOrchestratorRetryStormSingleRun",
		// WIRED queue leg (E19 T6): the bridge owns no retry of its own — the queue is the single owner.
		"apps/control-plane/internal/automation/queue_bridge_component_test.go:TestQueueBridgeLostAckRedeliversToExactlyOneRun",
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
	// escape proof resolution — OR in the ONE other catalog that owns some of them (uat.AgentSurfaceCaseIDs,
	// resolved by tests/uat/agent-surface). The sweep stays TOTAL: nothing is skipped, ownership is simply
	// allowed to live in two places, and TestTheSLKCatalogsAreDisjoint below refuses an id claimed by both.
	//
	// WHY E20's FOUR IDS ARE NOT IN THIS MAP, and it is a measured decision rather than a convenience: this
	// map IS the shipped extensions-0.1.0 bundle's case list (the generator reads it), and uat.CapabilityClaims
	// feeds a digest folded into that bundle's every checksum. Adding SLK-009..012 here would force the
	// regeneration of a committed HISTORICAL release, rewriting the record of a run that happened. A case
	// belongs to the bundle that certifies it, and these four are certified by slack-agent-surface-0.1.0.
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
				_, e17 := expectedExtensionsCatalog[e.Name()]
				if !e17 && !slices.Contains(uat.AgentSurfaceCaseIDs, e.Name()) &&
					!slices.Contains(uat.ToolsMemoryCaseIDs, e.Name()) {
					t.Errorf("%s: a case dir under a guarded prefix is in NONE of expectedExtensionsCatalog, uat.AgentSurfaceCaseIDs or uat.ToolsMemoryCaseIDs (add it to one, or it escapes proof resolution entirely)", e.Name())
				}
				break
			}
		}
	}
}

// TestTheSLKCatalogsAreDisjoint is the other half of the split ownership above, now THREE-WAY. Two catalogs
// resolving the same id would both report green while each assumed the other was the authority — and worse, a
// case could be quietly moved from the gate that actually runs its proofs to one that does not. Exactly one
// owner, and each downstream side (tests/uat/agent-surface, tests/uat/tools-memory) asserts every id in its
// own list has a directory.
func TestTheSLKCatalogsAreDisjoint(t *testing.T) {
	owners := map[string][]string{
		"uat.AgentSurfaceCaseIDs": uat.AgentSurfaceCaseIDs,
		"uat.ToolsMemoryCaseIDs":  uat.ToolsMemoryCaseIDs,
	}
	for name, ids := range owners {
		for _, id := range ids {
			if _, both := expectedExtensionsCatalog[id]; both {
				t.Errorf("%s is claimed by BOTH expectedExtensionsCatalog and %s — two owners is no owner", id, name)
			}
		}
	}
	for _, id := range uat.AgentSurfaceCaseIDs {
		if slices.Contains(uat.ToolsMemoryCaseIDs, id) {
			t.Errorf("%s is claimed by BOTH uat.AgentSurfaceCaseIDs and uat.ToolsMemoryCaseIDs — two owners is no owner", id)
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

// ledgerExemptCases are the E17 cases that legitimately belong to NO capability's claim ledger, each with the
// reason. Everything else must be governed: the two sides are checked in BOTH directions, so a capability
// deleted from the tier table AND its cases quietly dropped from the catalog cannot vanish without a failure.
var ledgerExemptCases = map[string]string{
	"AUT-013": "the orchestrator KIT leg (plan §T8) — SDK + conformance fixtures + a scripted fake orchestrator; §T8 states it is explicitly NOT a discovery capability, so no tier is computed over it",
	"QUA-001": "eval-harness machinery (plan §T6): release gating, not a discovery capability",
	"QUA-002": "eval-harness machinery (plan §T6): the model-judge calibration smoke, not a discovery capability",
	"QUA-004": "the eval GATE half (plan §T6) — carried by EvalGateProof through EvalPromoteGate, not by a capability tier",
}

// TestEveryCatalogedCaseIsGovernedOrExempt is the REVERSE of TestEveryCapabilityClaimIsACatalogedCase. That one
// stops a tier being computed over a claim nobody proves; this one stops a proven case drifting OUT of every
// capability's ledger. Without it, deleting a capability from CapabilityTierOrder + CapabilityClaims and
// leaving its cases in the catalog would silently ungovern them — both sides would agree, and nothing would
// fail. QUA-003 is governed as the security PRECONDITION rather than by ownership, so allClaimCases covers it.
func TestEveryCatalogedCaseIsGovernedOrExempt(t *testing.T) {
	governed := map[string]bool{}
	for _, id := range allClaimCases() {
		governed[id] = true
	}
	for id := range expectedExtensionsCatalog {
		if governed[id] {
			if why, exempt := ledgerExemptCases[id]; exempt {
				t.Errorf("%s is BOTH in a capability's claim ledger and on the exempt list (%q) — the exemption is stale, drop it", id, why)
			}
			continue
		}
		if _, exempt := ledgerExemptCases[id]; !exempt {
			t.Errorf("%s is a cataloged E17 case but NO capability's claim ledger governs it and it is not on the explicit exempt list — a capability deleted from both sides must not vanish silently (add it to uat.CapabilityClaims, or to ledgerExemptCases with the reason)", id)
		}
	}
	for id := range ledgerExemptCases {
		if _, cataloged := expectedExtensionsCatalog[id]; !cataloged {
			t.Errorf("%s is on the ledger-exempt list but is not a cataloged E17 case — a stale exemption hides a deletion", id)
		}
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no build tag
// runs in make verify / test-unit, so it is the "unit" tier. A .ts file is a playwright spec and a .mjs file is
// the E19 T7 fake-vs-real conformance sweep (node:test): both are the e2e-deterministic tier, driven by the
// console's own runners rather than `go test`.
func buildClass(path, body string) string {
	if isConsoleProof(path) {
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
		case isConsoleProof(file):
			if !strings.Contains(body, name) {
				t.Errorf("%s: console test %q not found in %s (the case claims a test that is not in the tree)", id, name, file)
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

// isConsoleProof reports whether a proof reference names one of the console's own test files rather than a Go
// test: a .ts playwright spec, or the .mjs fake-vs-real conformance sweep (E19 T7, node:test). Both resolve by
// TITLE substring because neither declares a Go func, and both are driven by the console's runners.
func isConsoleProof(path string) bool {
	return strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".mjs")
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
