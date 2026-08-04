package agentsurface

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

// The E20 CATALOG GATE. It owns the four case ids this epic opened, and it owns them ALONE — the E17 gate's
// orphan sweep defers to uat.AgentSurfaceCaseIDs and refuses any id claimed by both, so ownership moved
// without lapsing. Everything the E17 catalog gate enforces is enforced here too: the dir exists, the id
// matches the directory, the name is a kebab-case behaviour assertion, the declared class is a §10.2 class
// its proofs actually support, every referenced proof RESOLVES in the tree, and the case text names both its
// LOCAL SEAM and its §6 operator leg.
//
// ponytail: this is a sixth copy-adaptation of tests/uat/automation/catalog_test.go's quartet. Hoisting the
// shared half into a package is a separate refactor, and doing it from an EXIT-gate task would put five
// other gates at risk for a tidiness win.

type surfaceCase struct {
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

// expectedAgentSurfaceCatalog is the E20 UAT catalog: the four ids this epic materializes (plan §T5) mapped
// to the proof class their case.yaml must declare and the in-tree proofs that prove them. A missing dir, a
// drifted class, a changed proof list, or a proof reference that does not resolve fails the gate.
var expectedAgentSurfaceCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// E20 T1 — the stream. The claims that rot most easily are the two negatives (a redelivery opens no
	// second stream; stopping a stream does not stop a run), so both halves of each are listed: the
	// behavioural proof against real Postgres AND the structural source scan that fails if the follower ever
	// so much as names AcceptCommand.
	"SLK-009": {"unit", []string{
		"adapters/integrations/slack/stream_test.go:TestStartStreamRefusesWithoutARecipient",
		"adapters/integrations/slack/stream_test.go:TestStartStreamCarriesTheDocumentedArguments",
		"adapters/integrations/slack/stream_test.go:TestOnlyStopStreamCarriesBlocks",
		"adapters/integrations/slack/stream_test.go:TestAppendStreamSurfacesStoppedByUser",
		"adapters/integrations/slack/stream_test.go:TestStreamTextIsTruncatedVisibly",
		"adapters/integrations/slack/agent_test.go:TestSetStatusCarriesTheDocumentedArguments",
		"adapters/integrations/slack/agent_test.go:TestSetStatusCapsLoadingMessagesAtTen",
	}},
	// E20 T2 — the panel. The DM exemption is a WIDENING, so its three legs are listed together with the
	// manifest gate that refuses a subscribed event whose scope the manifest does not grant.
	"SLK-010": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventPanelSurfaceEventsBirthNoRun",
		"adapters/integrations/slack/inbound_test.go:TestMapEventCarriesTheDMChannelType",
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsTheBotsOwnDM",
		"adapters/integrations/slack/manifest_test.go:TestManifestSubscribesToNothingTheAdapterHasNotDecided",
		"adapters/integrations/slack/manifest_test.go:TestManifestAgentViewHonoursThePublishedLimits",
		"adapters/integrations/slack/manifest_test.go:TestManifestGrantsTheScopesTheSubscribedEventsRequire",
	}},
	// E20 T3 — the context. Four authority boundaries, each with its own proof, plus the AST guard that
	// covers the half a behavioural zero-call assertion cannot.
	"SLK-011": {"unit", []string{
		"adapters/integrations/slack/inbound_test.go:TestMapEventCarriesAppContextInRelevanceOrder",
		"adapters/integrations/slack/inbound_test.go:TestMapEventDropsForeignWorkspaceContextEntities",
		"adapters/integrations/slack/inbound_test.go:TestMapEventSurvivesAStructuredContextValue",
	}},
	// E20 T4 — the renderer. The claim is negative ("the model cannot mint an actionable element"), the kind
	// that rots into a vacuous green, so the proof list carries BOTH halves: the sweep that finds nothing in
	// the renderer's output, and the sweep pointed at ApprovalMessage that proves it can find something.
	// Drop the second and the first certifies nothing.
	"SLK-012": {"unit", []string{
		"adapters/integrations/slack/blocks_test.go:TestRenderRefusesToMintAnActionableElementFromModelOutput",
		"adapters/integrations/slack/blocks_test.go:TestApprovalMessageIsTheOnlyMintOfAnActionableElement",
		"adapters/integrations/slack/blocks_test.go:TestNoFileButInteractionsMintsAnActionableElement",
		"adapters/integrations/slack/blocks_test.go:TestUnknownVariantRendersAsInertText",
		"adapters/integrations/slack/blocks_test.go:TestTaskStatusMapIsExplicitAndFailsClosed",
		"adapters/integrations/slack/blocks_test.go:TestOneTaskIsACardAndSeveralAreAPlan",
		"adapters/integrations/slack/blocks_test.go:TestTableTruncationIsVisible",
		"adapters/integrations/slack/blocks_test.go:TestBlockLimitIsVisible",
		"adapters/integrations/slack/blocks_test.go:TestBlocksNeutralizeBroadcastTokens",
		"adapters/integrations/slack/blocks_test.go:TestFileRefLinksOnlyHTTPAndHTTPS",
		// E21 T6 — the markdown block. Each leg is the strong form of a claim a weaker one would fake: the
		// neutralisation sweep DECODES before it looks, the budget fixture is three under-limit parts that are
		// over the limit together, and the stopStream fence is shown to discriminate against the postMessage
		// surface so it cannot rot into a green that certifies nothing.
		"adapters/integrations/slack/blocks_test.go:TestPostMessageRendersProseAsAMarkdownBlock",
		"adapters/integrations/slack/blocks_test.go:TestMarkdownBlockNeutralizesBroadcastTokens",
		"adapters/integrations/slack/blocks_test.go:TestMarkdownBudgetIsCumulativeAndCutsVisibly",
		"adapters/integrations/slack/blocks_test.go:TestTheStopStreamPathCarriesNoMarkdownBlock",
		"adapters/integrations/slack/blocks_test.go:TestATableCellIsNeverTypedRawNumber",
		// E23 T4 — the WIDGET HALF of this epic's approval screen, and it EXTENDS this case rather than
		// opening an id: what E20 proved is that the model cannot mint an actionable element, and E23 added
		// the largest set of actionable elements this tree has ever built beside that claim. A third button
		// (Show arguments) is minted with the other two and carries the SAME one-shot request hash; it can
		// never become a decision, because MapInteractiveApproval's switch matches approve/deny alone, and
		// that is asserted from BOTH directions rather than trusted. The rejected blocks — card (its `body`
		// is 200 characters), carousel, container, feedback_buttons, icon_button, context_actions — are
		// asserted ABSENT from both surfaces, so a rejection recorded in a plan is also a rejection a test
		// can see. And the singularity scan's own CEILING gets a test with a NAME (§3.6 D13: it reads one
		// directory, not the module), because the modal view could have been built one package over and left
		// that scan green while the claim it certifies was false.
		"adapters/integrations/slack/approval_widgets_test.go:TestApprovalMessageMintsThreeButtonsAllBoundToTheRequestHash",
		"adapters/integrations/slack/approval_widgets_test.go:TestShowArgumentsIsMappedButCanNeverBecomeADecision",
		"adapters/integrations/slack/approval_widgets_test.go:TestTheRejectedBlocksAreAbsentFromBothSurfaces",
		"adapters/integrations/slack/blocks_test.go:TestTheActionableElementScanIsPackageLocalOnlyAndTheModalIsBuiltInsideIt",
	}},
}

// caseAssertion is the bundle's per-case sentence, DERIVED from this catalog so a bundle assertion can never
// describe a proof list the case does not declare.
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedAgentSurfaceCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; "))
}

// TestAgentSurfaceCatalogMaterialized is the E20 catalog gate. It rides make verify (no Docker, no
// credential), so a forgotten case, a vanished proof leg, or a case text that hides which seam produced its
// evidence fails fast.
func TestAgentSurfaceCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedAgentSurfaceCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c surfaceCase
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
		assertSurfaceProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// The E20 side of the split ownership: every id this package claims must actually have a directory. The
	// E17 gate covers the other direction (no SLK- dir escapes both catalogs).
	for _, id := range uat.AgentSurfaceCaseIDs {
		if _, ok := expectedAgentSurfaceCatalog[id]; !ok {
			t.Errorf("%s is in uat.AgentSurfaceCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedAgentSurfaceCatalog {
		if !slices.Contains(uat.AgentSurfaceCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.AgentSurfaceCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}
}

// assertSurfaceProofs resolves every referenced proof in the tree and refuses a tier overclaim: a case
// declaring component-real must reference at least one proof carrying the `component` build tag, or it is
// claiming a real backing service it never touched.
func assertSurfaceProofs(t *testing.T, root, id, class string, want, got []string) {
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

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no tag
// runs in make verify, so it is the "unit" tier.
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
