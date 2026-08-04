package toolapproval

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

// The E23 CATALOG GATE. It owns the five case ids this epic opened, and it owns them ALONE — the E17 gate's
// orphan sweep defers to uat.ToolApprovalCaseIDs and refuses an id claimed by more than one catalog, so
// ownership moved without lapsing. Everything the E17/E20/E21/E22 catalog gates enforce is enforced here
// too: the dir exists, the id matches the directory, the name is a kebab-case behaviour assertion, the
// declared class is a §10.2 class its proofs actually support, every referenced proof RESOLVES in the tree,
// and the case text names both its LOCAL SEAM and its §6 operator leg.
//
// ponytail: this is a ninth copy-adaptation of tests/uat/automation/catalog_test.go's quartet. Hoisting the
// shared half into a package is a separate refactor, and doing it from an EXIT-gate task would put eight
// other gates at risk for a tidiness win.

type hilCase struct {
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

// expectedToolApprovalCatalog is the E23 UAT catalog: the five ids this epic materializes (plan §T7) mapped
// to the proof class their case.yaml must declare and the in-tree proofs that prove them. A missing dir, a
// drifted class, a changed proof list, or a proof reference that does not resolve fails the gate.
//
// EVERY LIST CARRIES BOTH HALVES OF ITS NEGATIVE, which is this family's standing rule: a claim shaped "X
// cannot happen" is the kind that rots into a vacuous green, so the list also names the proof that shows the
// guard can FIRE. HIL-001 carries the ungated tool that is bit-unchanged beside the gated one that stops;
// HIL-003 carries the terminal run that STILL refuses a decision beside the parked one that accepts it;
// HIL-004 carries the unconfigured project that behaves exactly as before beside the list that refuses.
var expectedToolApprovalCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// E23 T1/T5 — the gate itself. The load-bearing pair is the FIRST and the FIFTH: a gated call that never
	// reaches Execute, and an UNGATED tool whose behaviour is bit-unchanged. Without the second, a build that
	// blocked every tool would satisfy the first.
	"HIL-001": {"component-real", []string{
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalGateNeverReachesExecuteWithoutAHumanDecision",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalApprovedCallRunsExactlyOnceAndWakesTheRun",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalDoesNotRunWhenArgumentsChangedAfterTheApproval",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalDenyIsAnAnswerTheModelContinuesOn",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolWithoutApprovalDeclaredIsBitUnchanged",
		"apps/control-plane/internal/execution/mcp_write_approval_component_test.go:TestAGatedMCPWriteToolSendsNoRequestWithoutAHuman",
		"apps/control-plane/internal/execution/mcp_write_approval_component_test.go:TestAToolPinnedTwiceRefusesRatherThanCoinFlippingTheGate",
	}},
	// E23 T1/T4 — the screen. The untagged derivation legs are listed with the two renderers and the route,
	// because the claim has three layers and each can rot on its own: what the display COMPUTES, what the two
	// surfaces RENDER from it, and what the modal route does inside Slack's three-second budget.
	"HIL-002": {"component-real", []string{
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenCarriesNoServerOrModelText",
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenShowsEveryArgumentKeySorted",
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenNamesAnAbsentOperatorLabel",
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenTruncatesVisibly",
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenIsDerivedNotStored",
		"apps/control-plane/internal/execution/approval_display_test.go:TestToolApprovalScreenNeutralizesBroadcasts",
		"adapters/integrations/slack/approval_widgets_test.go:TestNeitherSurfaceShowsAServerDescriptionOrModelProse",
		"adapters/integrations/slack/approval_widgets_test.go:TestApprovalMessageShowsEveryArgumentOrSaysItCutOne",
		"adapters/integrations/slack/approval_widgets_test.go:TestApprovalMessageMarksATruncatedArgumentSetRatherThanDroppingIt",
		"adapters/integrations/slack/approval_widgets_test.go:TestAnUnwrittenOperatorLabelIsSaidOutLoud",
		"adapters/integrations/slack/approval_widgets_test.go:TestApprovalModalDumpsEveryLeafAndAsksForADenyReasonWithoutDispatching",
		"adapters/integrations/slack/blocks_test.go:TestTheActionableElementScanIsPackageLocalOnlyAndTheModalIsBuiltInsideIt",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalScreenComesFromTheLedgerRowAndNotTheFrame",
	}},
	// E23 T1/T3 — the park and its deadline. The expiry leg is the one with no prior art in this tree, and the
	// LAST entry is what keeps the claim honest rather than triumphant: a genuinely terminal run still refuses
	// the click and still answers 503, deliberately.
	"HIL-003": {"component-real", []string{
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestToolApprovalParksTheRunRatherThanAnsweringWithoutADecision",
		"apps/control-plane/internal/execution/tool_approval_component_test.go:TestExpiredToolApprovalCancelsTheCallAndWakesTheParkedRun",
		"apps/control-plane/internal/execution/reconciler_test.go:TestReconcilerSweepReportsDeadLetteredWithConfiguredCeiling",
		// RE-EARNED 2026-08-05 — the park and the wake, off Slack.
		"apps/control-plane/internal/execution/publish_approval_component_test.go:TestPublicationParksTheRunSoTheApproveLands",
		"apps/control-plane/internal/execution/publish_approval_component_test.go:TestPublicationDenyWakesTheParkedRunAndPublishesNothing",
	}},
	// E23 T2 — who may decide. Both surfaces are listed because the whole design claim is that they refuse in
	// the SAME function, and the bit-unchanged legs are listed because an approver list that changed the
	// behaviour of an unconfigured deployment would be a silent breaking change dressed as a security fix.
	"HIL-004": {"component-real", []string{
		"packages/coordinator/approver_test.go:TestApproverPrincipalRendersEachSurfacesIdentity",
		"packages/coordinator/approver_test.go:TestApproverAllowedIsDenyByDefaultOnlyOnceAListExists",
		"packages/coordinator/approver_test.go:TestApproverAllowedHasExactlyOneProductionCallSite",
		"apps/control-plane/internal/identity/config_policy_test.go:TestConfigPolicyInputAcceptsAnApproverList",
		"apps/control-plane/internal/identity/config_policy_test.go:TestConfigPolicyInputStillRejectsAnUnknownField",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverAnyTenantKeyApprovesWhenNoListIsConfigured",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverAKeyOutsideTheProjectListDecidesNothing",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverAKeyInsideTheProjectListApproves",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverADenyFromAnUnlistedKeyDecidesNothingEither",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverTheRequestBodyCannotNameItsOwnApprover",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverTheListIsReadLiveSoNarrowingTakesPendingApprovalsWithIt",
		"apps/control-plane/internal/execution/approver_component_test.go:TestApproverAnEmptyListIsNotAnEmptyString",
		"cmd/cli/internal/stack/up_approver_test.go:TestApproverListAbsenceIsSaidOutLoud",
		"cmd/cli/internal/stack/up_approver_test.go:TestApproverListPresenceIsSilent",
		"cmd/cli/internal/stack/up_approver_test.go:TestApproverListUnreadableWarnsAboutTheREADNotTheLIST",
	}},
	// E23 T5/T6 — the approved write actually landing, on two kinds of counterparty. The live leg is listed
	// because "GitHub really answers 409 to a moved head" is the one claim no fake can close: a fake can only
	// echo back the rule we wrote into it.
	"HIL-005": {"component-real", []string{
		"apps/control-plane/internal/execution/mcp_write_approval_component_test.go:TestApprovedMCPArgumentsReachThePeerByteForByte",
		"apps/control-plane/internal/execution/mcp_write_approval_component_test.go:TestAJiraTicketBodyCannotApproveItself",
		"apps/control-plane/internal/execution/merge_component_test.go:TestMergePullRequestWithoutAnApprovalPublishesNothing",
		"apps/control-plane/internal/execution/merge_component_test.go:TestMergePullRequestRefusesWhenTheHeadMovedAfterTheApproval",
		"apps/control-plane/internal/execution/merge_component_test.go:TestMergePullRequestWithNoPublishedPullRequestRefuses",
		"apps/control-plane/internal/execution/merge_component_test.go:TestMergePullRequestDeniedPreventsTheMergeAndReleasesTheRun",
		"apps/control-plane/internal/execution/merge_component_test.go:TestMergePullRequestMethodComesFromTheBindingNotTheModel",
		"apps/control-plane/internal/execution/tools/publish_test.go:TestNoPublishToolLetsTheModelNameTheDestination",
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestThePublishToolsAreTheirOwnListAndNeitherPublishes",
		"adapters/repositories/publish_github_test.go:TestGitHubMergeAlwaysSendsTheApprovedSHA",
		"adapters/repositories/publish_github_test.go:TestGitHubMergeRefusalIsHonest",
		"adapters/repositories/publish_github_test.go:TestMergeRefusesAnUnknownMethodBeforeCalling",
		"tests/live/repository/live_test.go:TestLiveApprovedMergeRefusesAMovedHead",
	}},
}

// caseAssertion is the bundle's per-case sentence, DERIVED from this catalog so a bundle assertion can never
// describe a proof list the case does not declare.
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedToolApprovalCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; "))
}

// TestToolApprovalCatalogMaterialized is the E23 catalog gate. It rides make verify (no Docker, no
// credential), so a forgotten case, a vanished proof leg, or a case text that hides which seam produced its
// evidence fails fast.
func TestToolApprovalCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedToolApprovalCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c hilCase
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
		assertHILProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// The E23 side of the split ownership: every id this package claims must actually have a directory. The
	// E17 gate covers the other direction (no dir under a guarded prefix escapes every catalog).
	for _, id := range uat.ToolApprovalCaseIDs {
		if _, ok := expectedToolApprovalCatalog[id]; !ok {
			t.Errorf("%s is in uat.ToolApprovalCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedToolApprovalCatalog {
		if !slices.Contains(uat.ToolApprovalCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.ToolApprovalCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}
}

// TestEveryHILDirectoryIsOwned is the E23 half of the orphan rule, asserted HERE as well as in the E17 gate.
// Two gates checking it is not redundancy: the E17 sweep walks the directory and defers to this epic's list,
// so if somebody deleted the `HIL-` entry from extensionIDPrefixes the sweep would go quiet and stay green —
// which is EXACTLY the state this task found the tree in, because HIL-004 shipped in T2 while `HIL-` was in
// no prefix list at all. This one walks the same directory and cannot be silenced by that edit.
func TestEveryHILDirectoryIsOwned(t *testing.T) {
	casesDir := filepath.Join(repoRoot(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "HIL-") {
			continue
		}
		seen++
		if _, ok := expectedToolApprovalCatalog[e.Name()]; !ok {
			t.Errorf("%s is a HIL- case dir that NO catalog resolves proofs for — it escapes proof resolution entirely", e.Name())
		}
	}
	if seen != len(uat.ToolApprovalCaseIDs) {
		t.Errorf("the tree holds %d HIL- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.ToolApprovalCaseIDs))
	}
}

// TestTheHILPrefixIsGuardedBySweep is the guard for the guard, and it exists because of what this task
// measured rather than as a precaution. T2 shipped tests/uat/cases/HIL-004 while `HIL-` was in NO prefix
// list, so that directory escaped the ONLY sweep in the tree that walks the cases directory — a case with a
// proof list nothing resolved, reported green by silence. The prefix is in extensionIDPrefixes now, and this
// asserts it stays there, because deleting one string from that slice would restore the hole without
// failing anything else.
func TestTheHILPrefixIsGuardedBySweep(t *testing.T) {
	path := filepath.Join(repoRoot(t), "tests", "uat", "extensions", "catalog_test.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the E17 orphan sweep: %v", err)
	}
	body := string(raw)
	if !strings.Contains(body, `"HIL-"`) {
		t.Error(`extensionIDPrefixes no longer contains "HIL-" — the E17 sweep is the only place in the tree that walks tests/uat/cases, so a prefix outside it is a family whose directories NOTHING checks; ownership may live in uat.ToolApprovalCaseIDs, escaping the sweep may not`)
	}
	if !strings.Contains(body, "uat.ToolApprovalCaseIDs") {
		t.Error("the E17 orphan sweep does not defer to uat.ToolApprovalCaseIDs — with the prefix guarded but the ownership list unknown to it, every HIL- dir would be reported as an escapee and the sweep would be turned off again to silence it")
	}
}

// assertHILProofs resolves every referenced proof in the tree and refuses a tier overclaim: a case declaring
// component-real must reference at least one proof carrying the `component` build tag, or it is claiming a
// real backing service it never touched.
func assertHILProofs(t *testing.T, root, id, class string, want, got []string) {
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
