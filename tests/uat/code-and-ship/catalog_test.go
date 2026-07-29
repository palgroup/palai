package codeandship

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/palgroup/palai/tests/uat"
)

// The E22 CATALOG GATE. It owns the five case ids this epic opened, and it owns them ALONE — the E17 gate's
// orphan sweep defers to uat.CodeAndShipCaseIDs and refuses an id claimed by more than one catalog, so
// ownership moved without lapsing. Everything the E17/E20/E21 catalog gates enforce is enforced here too:
// the dir exists, the id matches the directory, the name is a kebab-case behaviour assertion, the declared
// class is a §10.2 class its proofs actually support, every referenced proof RESOLVES in the tree, and the
// case text names both its LOCAL SEAM and its §6 operator leg.
//
// ponytail: this is an eighth copy-adaptation of tests/uat/automation/catalog_test.go's quartet. Hoisting the
// shared half into a package is a separate refactor, and doing it from an EXIT-gate task would put seven
// other gates at risk for a tidiness win.

type shipCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

var (
	honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	validProofClasses = map[string]bool{
		"unit": true, "component-real": true, "e2e-deterministic": true,
		"live-provider": true, "external-receipt": true, "fault-live": true,
	}
	// dockerBoundClasses are the classes whose //go:build TAG is load-bearing: a case declaring one must
	// reference at least one proof carrying that exact tag, or it claims a backing service it never touched.
	dockerBoundClasses = map[string]bool{"component-real": true, "live-provider": true, "fault-live": true}
	// localSeamWords are the words that tell a reader WHAT produced the evidence. A case that names none of
	// them is the prose half of an overclaim.
	localSeamWords = []string{"fake", "loopback", "deterministic", "live", "component-real", "local proof", "local seam"}
)

// expectedCodeAndShipCatalog is the E22 UAT catalog: the five ids this epic materializes (plan §T7) mapped to
// the proof class their case.yaml must declare and the in-tree proofs that prove them. A missing dir, a
// drifted class, a changed proof list, or a proof reference that does not resolve fails the gate.
//
// EVERY LIST CARRIES BOTH HALVES OF ITS NEGATIVE, which is this family's standing rule: a claim shaped "X
// cannot happen" is the kind that rots into a vacuous green, so the list also names the proof that shows the
// guard can FIRE. CAS-001 carries the bit-unchanged connection beside the bound one; CAS-002 carries the deny
// that prevents beside the approve that publishes; CAS-005 carries the clean refusal with no posture
// configured beside the host that answers `Xcode 26.6`.
var expectedCodeAndShipCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// E22 T3 — the repository binding. The claim that rots is "a thread can reach a repo": worthless without
	// "and a connection WITHOUT one is bit-unchanged" and "another tenant's ref is not found". All three are
	// listed, plus the dead-tool guard, because granting a tool whose every call fails is the failure mode
	// §3.6 D4 measured.
	"CAS-001": {"component-real", []string{
		"apps/control-plane/api/slack_connections_test.go:TestSlackDefaultPolicyAcceptsARepositoryAndStaysClosed",
		"apps/control-plane/api/slack_connections_test.go:TestSlackDefaultPolicyWithoutARepositoryIsBitUnchanged",
		"apps/control-plane/api/slack_connections_test.go:TestSlackDefaultPolicyRefusesARefWithNoBinding",
		"apps/control-plane/api/slack_connections_test.go:TestSlackDefaultPolicyPatchBindsARepositoryToAnExistingConnection",
		"apps/control-plane/internal/store/slack_repository_component_test.go:TestSlackRunCarriesTheConnectionsRepositoryBinding",
		"apps/control-plane/internal/store/slack_repository_component_test.go:TestSlackConnectionWithoutARepositoryIsBitUnchanged",
		"apps/control-plane/internal/store/slack_repository_component_test.go:TestSlackRefusesAnotherTenantsRepositoryBinding",
		"cmd/cli/internal/stack/up_repository_test.go:TestABringUpGrantsWorkspaceToolsOnlyWithARepositoryBinding",
		"cmd/cli/internal/stack/up_repository_test.go:TestABringUpBindsTheRepositoryAndSaysWhatItMade",
		"cmd/cli/internal/stack/up_repository_test.go:TestABringUpReusesTheRepositoryBindingItAlreadyMade",
		"cmd/cli/internal/stack/up_repository_test.go:TestAMissingRepositoryConfigurationWarnsRatherThanSkippingSilently",
		"cmd/cli/internal/stack/up_repository_test.go:TestARepositorySurfaceTheStackDoesNotMountIsReportedRatherThanSwallowed",
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestEverySlackDefaultToolResolves",
		"tests/live/repository/live_test.go:TestLiveSlackBoundRepositoryClonesAtItsBaseBranch",
		"tests/live/workspace/live_test.go:TestLiveSlackBoundRepositoryLandsWhereTheWorkspaceToolsLook",
	}},
	// E22 T4 — the publication, EXTENDED BY E23 T3. The load-bearing pair is still the FIRST two: an approve
	// that publishes exactly once, and a deny after which the publisher is never asked for anything at all.
	// The schema guard is third because it is the structural half — a base the model can choose is a base
	// nobody approved. The four E23 entries are what turned E22's two measured ceilings into behaviour: the
	// approval message is POSTED, the run PARKS so the approve lands instead of answering 503, a deny
	// releases the run, and a delivery Slack refuses costs the button and never the approval or the run. The
	// LAST one is the half of E22's ceiling that is still true and is asserted on its own rather than
	// dropped — a genuinely terminal run still refuses the click.
	"CAS-002": {"component-real", []string{
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackWaitsForAnApproveAndThenPublishes",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackDenialPreventsThePushEntirely",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackTargetsTheBindingsBaseBranch",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackPostsAnApprovalMessageIntoTheThread",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackParksTheRunSoTheApproveLands",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackDenyWakesTheParkedRunAndPublishesNothing",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackDeliveryFailureKeepsTheApprovalAndReleasesTheRun",
		"apps/control-plane/internal/execution/slack_publish_component_test.go:TestPublicationFromSlackCeilingATerminalRunStillRefusesTheClick",
		"apps/control-plane/internal/execution/tools/publish_test.go:TestNoPublishToolLetsTheModelNameTheDestination",
		"apps/control-plane/internal/execution/tools/publish_test.go:TestPushToolRecordsPendingPublicationAtWorkspaceHead",
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestThePublishToolsAreTheirOwnListAndNeitherPublishes",
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestEverySlackDefaultToolResolves",
		"cmd/cli/internal/stack/up_repository_test.go:TestABringUpWithARepositoryGrantsThePublishToolsAndOneWithoutGrantsNone",
		"cmd/cli/internal/stack/up_publisher_test.go:TestABoundRepositoryWithNoGitHubAppWarnsRatherThanWaitingForever",
		"cmd/cli/internal/stack/up_publisher_test.go:TestAHalfConfiguredGitHubAppIsRefusedByName",
		"cmd/cli/internal/stack/up_publisher_test.go:TestTheGitHubAppKeyRidesAFileSecretAndTheEnvironmentCarriesOnlyAPath",
		"cmd/cli/internal/stack/up_publisher_test.go:TestComposeMountsTheGitHubAppKeyAsAFileSecret",
		"adapters/integrations/slack/blocks_test.go:TestApprovalMessageIsTheOnlyMintOfAnActionableElement",
		"apps/control-plane/internal/store/slack_interactions_component_test.go:TestSlackUnauthorizedClickEnqueuesNothing",
		"tests/live/repository/live_test.go:TestLiveApprovedPushAndDraftPullRequest",
	}},
	// E22 T6 — the Jira rider and the ticket body. One component test carries all five refusals because they
	// are five re-derivations over ONE call; the rider tests are what make the surface reachable at all, and
	// the live leg asserts a tool BY NAME because an unaccepted credential does not answer 401 (X16 J5).
	"CAS-003": {"component-real", []string{
		"apps/control-plane/internal/extensions/jira_ticket_injection_component_test.go:TestJiraTicketBodyCannotInstructTheAgent",
		"cmd/cli/internal/stack/up_mcp_test.go:TestABringUpNamesNoMCPConnectionByDefault",
		"cmd/cli/internal/stack/up_mcp_test.go:TestAnOperatorNamesMCPConnectionsByName",
		"cmd/cli/internal/stack/up_mcp_test.go:TestNoneDisarmsTheMCPRiderAndBlankIsUnset",
		"cmd/cli/internal/stack/up_mcp_test.go:TestChangingTheMCPRiderMintsANewRevisionRatherThanSilentlyReusingTheOld",
		"cmd/cli/internal/stack/up_mcp_test.go:TestReorderingTheMCPRiderIsNotAChange",
		"cmd/cli/internal/stack/up_mcp_test.go:TestTheBringUpSaysWhenNoMCPConnectionIsNamed",
		"adapters/integrations/mcp/jira_live_test.go:TestLiveJiraMCPServerReachableAndEnumerable",
	}},
	// E22 T5 — the upload. The extension-from-content test is the one to read: `simctl io recordVideo
	// --codec=h264` writes a QuickTime container, so a model calling its recording demo.mp4 publishes nothing.
	"CAS-004": {"component-real", []string{
		"adapters/integrations/slack/upload_test.go:TestUploadToThreadFollowsTheThreeDocumentedSteps",
		"adapters/integrations/slack/upload_test.go:TestUploadCarriesTheModelsTextOnlyInInitialComment",
		"adapters/integrations/slack/upload_test.go:TestSniffUploadDerivesTheExtensionFromContentNotFromAName",
		"adapters/integrations/slack/upload_test.go:TestUploadRefusesAnArtifactOverOurOwnCeiling",
		"adapters/integrations/slack/upload_test.go:TestUploadDoesNotRetryARateLimitedStep",
		"adapters/integrations/slack/upload_test.go:TestUploadSurfacesADocumentedAPIRefusal",
		"adapters/integrations/slack/blocks_test.go:TestTaskCardCarriesItsSourcesAndValidatesEveryURL",
		"adapters/integrations/slack/blocks_test.go:TestTaskCardSourceTextIsNeutralised",
		"adapters/integrations/slack/blocks_test.go:TestApprovalMessageIsTheOnlyMintOfAnActionableElement",
		"adapters/integrations/slack/manifest_test.go:TestManifestGrantsTheScopesTheSubscribedEventsRequire",
		"apps/control-plane/internal/extensions/slack_upload_test.go:TestArtifactIDIsRecognisedByShapeAndNearMissesAreRefused",
		"apps/control-plane/internal/extensions/slack_upload_test.go:TestArtifactRefsComeOnlyFromFileRefsInOrderAndDeduplicated",
		"apps/control-plane/internal/extensions/slack_upload_test.go:TestUploadNoteSaysWhatWasNotAttached",
		"apps/control-plane/cmd/palai-control-plane/main_test.go:TestSlackImageLegIsMountedWhenThereIsAnObjectStore",
		"apps/control-plane/internal/artifacts/inbound_image_component_test.go:TestReadRunArtifactRefusesForeignRunsForeignTenantsAndOversize",
		"apps/control-plane/internal/store/slack_upload_component_test.go:TestSlackRunArtifactReachesTheThreadAsAFile",
		"apps/control-plane/internal/store/slack_upload_component_test.go:TestSlackUploadRefusesAnArtifactFromAnotherRun",
		"apps/control-plane/internal/store/slack_upload_component_test.go:TestSlackUploadRefusesAnOversizeArtifactAndSaysSo",
		"apps/control-plane/internal/store/slack_upload_component_test.go:TestSlackUploadFailureLeavesTheAnswerAndTheRunAlone",
		"apps/control-plane/internal/store/slack_upload_component_test.go:TestSlackOrdinaryAnswerUploadsNothing",
		"tests/live/code-and-ship/upload_live_test.go:TestLiveSlackUploadsAScreenshotAndARecording",
	}},
	// E22 T1/T2 — the host, and the boundary this epic DELETES. The three live legs are listed because the
	// claim "the agent runs the host's own tools" is the one no deterministic tier can close on its own: only
	// a real Xcode and a real simulator can answer it, and they are hardware-gated rather than credentialed.
	"CAS-005": {"component-real", []string{
		"apps/control-plane/internal/execution/tools/shell_host_component_test.go:TestNativeShellPostureRunsTheHostsOwnToolchain",
		"apps/control-plane/internal/execution/tools/shell_host_component_test.go:TestNativeShellPostureSeparatesConcurrentSessionsOnOneMac",
		"apps/control-plane/internal/execution/tools/shell_host_component_test.go:TestShellToolStillFailsCleanlyWithNoPostureConfigured",
		"adapters/sandboxes/host/exec_test.go:TestHostShellDropsTheOperatorsEnvironment",
		"adapters/sandboxes/host/exec_test.go:TestHostShellGivesConcurrentAllocationsDisjointSessionDirectories",
		"adapters/sandboxes/host/exec_test.go:TestSimctlSetIsAdvisoryNotEnforced",
		"adapters/sandboxes/host/exec_test.go:TestHostShellRefusesAReadOnlyAttempt",
		"adapters/sandboxes/host/exec_test.go:TestHostShellRunsInTheWorkspaceRootAndRedactsSecrets",
		"adapters/sandboxes/host/exec_test.go:TestHostShellWallTimeKillsTheWholeProcessGroup",
		"adapters/sandboxes/host/exec_test.go:TestHostShellReportsAMissingCommandAsExit127",
		"adapters/sandboxes/oci/workspace/allocation_test.go:TestSnapshotSkipsThePerSessionDirectoryAsASubtree",
		"apps/control-plane/cmd/palai-control-plane/main_test.go:TestShellPostureAcceptsOnlyTheStringThatSaysWhatItIs",
		"apps/control-plane/cmd/palai-control-plane/main_test.go:TestShellPostureRefusesBothSandboxImageAndNativeHost",
		"cmd/cli/internal/stack/native_test.go:TestNativeWorkspaceRootRefusesAWorldWritableParent",
		"cmd/cli/internal/stack/native_test.go:TestNativeWorkspaceRootResolvesToOneRealPathOnBothSides",
		"deploy/compose/native_control_plane_test.go:TestNativeOverlayBindsTheWorkspaceAtTheSameAbsolutePath",
		"tests/docs/palai_on_a_mac_test.go:TestTheMacOperatingRuleIsVerbatimInBothOperatorPages",
		"apps/control-plane/internal/execution/tools/live/mac_host_live_test.go:TestLiveMacHostDrivesASimulatorThroughShellCalls",
		"apps/control-plane/internal/execution/tools/live/mac_host_live_test.go:TestLiveMacHostBuildsAnXcodeProjectThroughShellCalls",
		"apps/control-plane/internal/execution/tools/live/mac_host_live_test.go:TestLiveMacHostHomeDoesNotSelectTheSimulatorDeviceSet",
	}},
}

// caseAssertion is the bundle's per-case sentence, DERIVED from this catalog so a bundle assertion can never
// describe a proof list the case does not declare.
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedCodeAndShipCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; "))
}

// TestCodeAndShipCatalogMaterialized is the E22 catalog gate. It rides make verify (no Docker, no
// credential), so a forgotten case, a vanished proof leg, or a case text that hides which seam produced its
// evidence fails fast.
func TestCodeAndShipCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedCodeAndShipCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c shipCase
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

		lower := strings.ToLower(c.Input)
		named := false
		for _, word := range localSeamWords {
			if strings.Contains(lower, word) {
				named = true
				break
			}
		}
		if !named {
			t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — without it a reader cannot tell a fake-peer proof from a real one", id, localSeamWords)
		}
		if !strings.Contains(c.Input, "§6") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover", id)
		}
		assertShipProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// The E22 side of the split ownership: every id this package claims must actually have a directory. The
	// E17 gate covers the other direction (no dir under a guarded prefix escapes every catalog).
	for _, id := range uat.CodeAndShipCaseIDs {
		if _, ok := expectedCodeAndShipCatalog[id]; !ok {
			t.Errorf("%s is in uat.CodeAndShipCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedCodeAndShipCatalog {
		if !slices.Contains(uat.CodeAndShipCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.CodeAndShipCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}
}

// TestEveryCASDirectoryIsOwned is the E22 half of the orphan rule, asserted HERE as well as in the E17 gate.
// Two gates checking it is not redundancy: the E17 sweep walks the directory and defers to this epic's list,
// so if somebody deleted the `CAS-` entry from extensionIDPrefixes the sweep would go quiet and stay green.
// This one walks the same directory and cannot be silenced by that edit.
func TestEveryCASDirectoryIsOwned(t *testing.T) {
	casesDir := filepath.Join(repoRoot(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "CAS-") {
			continue
		}
		seen++
		if _, ok := expectedCodeAndShipCatalog[e.Name()]; !ok {
			t.Errorf("%s is a CAS- case dir that NO catalog resolves proofs for — it escapes proof resolution entirely", e.Name())
		}
	}
	if seen != len(uat.CodeAndShipCaseIDs) {
		t.Errorf("the tree holds %d CAS- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.CodeAndShipCaseIDs))
	}
}

// assertShipProofs resolves every referenced proof in the tree and refuses a tier overclaim: a case declaring
// component-real must reference at least one proof carrying the `component` build tag, or it is claiming a
// real backing service it never touched.
func assertShipProofs(t *testing.T, root, id, class string, want, got []string) {
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
		if !strings.Contains(body, "func "+name+"(") {
			t.Errorf("%s: proof %q not found in %s (the case claims a proof that is not in the tree)", id, name, file)
		}
		tiers[buildClass(body)] = true
	}
	if dockerBoundClasses[class] && !tiers[class] {
		t.Errorf("%s: declares proof_class %q (a Docker/credential-bound tier) but references no proof carrying the matching //go:build tag — tier overclaim; referenced tiers were %v", id, class, tiers)
	}
	if !dockerBoundClasses[class] && !tiers["unit"] {
		t.Errorf("%s: declares proof_class %q but every referenced proof is Docker/credential-bound (%v) — the declared tier cannot actually run this case's proof", id, class, tiers)
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no tag runs
// in make verify, so it is the "unit" tier.
func buildClass(body string) string {
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
			}
		}
	}
	return "unit"
}
