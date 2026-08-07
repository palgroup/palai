package toolsmemory

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

// The E21 CATALOG GATE. It owns the five case ids this epic opened, and it owns them ALONE — the E17 gate's
// orphan sweep defers to uat.ToolsMemoryCaseIDs and refuses an id claimed by more than one catalog, so
// ownership moved without lapsing. Everything the E17/E20 catalog gates enforce is enforced here too: the dir
// exists, the id matches the directory, the name is a kebab-case behaviour assertion, the declared class is a
// §10.2 class its proofs actually support, every referenced proof RESOLVES in the tree, and the case text
// names both its LOCAL SEAM and its §6 operator leg.
//
// ponytail: this is a seventh copy-adaptation of tests/uat/automation/catalog_test.go's quartet. Hoisting the
// shared half into a package is a separate refactor, and doing it from an EXIT-gate task would put six other
// gates at risk for a tidiness win.

type memoryCase struct {
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

// expectedToolsMemoryCatalog is the E21 UAT catalog: the five ids this epic materializes (plan §T7) mapped to
// the proof class their case.yaml must declare and the in-tree proofs that prove them. A missing dir, a
// drifted class, a changed proof list, or a proof reference that does not resolve fails the gate.
//
// EVERY LIST CARRIES BOTH HALVES OF ITS NEGATIVE, which is this family's standing rule: a claim shaped "X
// cannot happen" is the kind that rots into a vacuous green, so the list also names the proof that shows the
// guard can FIRE. TLM-003 carries the mint test beside the escape test; TLM-004 carries the fail-closed rider
// beside the successful dispatch; TLM-005 carries the not-offered test beside the offered one.
var expectedToolsMemoryCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// E21 T1 — the context window. The two claims that rot are "it folds" (worthless without "and the same
	// input folds identically") and "overflow is classified" (worthless without the wrapping test), so both
	// pairs are listed, plus the `clear` command's HTTP surface and its durable half.
	"TLM-001": {"component-real", []string{
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryStaysWithinTheContextBudget",
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryFoldsDeterministically",
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryKeepsTheNewestTurnsVerbatim",
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryFoldIsVisible",
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryUnderBudgetIsUnchanged",
		"apps/control-plane/internal/execution/history_compaction_test.go:TestHistoryFoldsWhenTheBudgetIsUnknown",
		"apps/control-plane/internal/execution/context_overflow_test.go:TestContextOverflowIsClassifiedFromTheProviderCode",
		"apps/control-plane/internal/execution/context_overflow_test.go:TestContextOverflowErrorIsRecognisableThroughWrapping",
		"apps/control-plane/internal/execution/context_overflow_test.go:TestContextOverflowProjectsADiagnosableTerminal",
		"apps/control-plane/api/commands_clear_test.go:TestClearIsAcceptedAtTheHTTPSurface",
		"tests/component/postgres/clear_session_test.go:TestClearSessionEmptiesHistoryAndKeepsTheSessionAlive",
		"tests/component/postgres/clear_session_test.go:TestClearSessionIsIdempotentAndLeavesLiveWorkAlone",
	}},
	// E21 T2 — the operator foundation. The load-bearing one is the FIRST: a secret decrypted after a
	// force-recreate on a stack with no Slack credentials at all, which is the exact configuration the old
	// master-key pointer silently broke.
	"TLM-002": {"e2e-deterministic", []string{
		"cmd/cli/internal/stack/up_secret_e2e_test.go:TestASecretSurvivesARestartOnAStackWithNoSlackApp",
		"cmd/cli/internal/stack/up_operator_test.go:TestMasterKeyPointerIsNotInterpolatedFromTheInvokingShell",
		"cmd/cli/internal/stack/up_operator_test.go:TestEveryBringUpLeavesABootableMasterKey",
		// Claim (2)'s seven proofs were WITHDRAWN 2026-08-04: `1e5fc63e refactor(cli): bring-up prepares a
		// stack and nothing else` removed the registration responsibility they proved, and none survives
		// under any name. The withdrawal is recorded in TLM-002/case.yaml beside the list it edits. Claim
		// (1)'s two master-key proofs above still stand and still pass — that is this case's own cliff.
		"deploy/compose/slack_wiring_test.go:TestComposeCanRedeemASecretRefHandle",
		"adapters/integrations/mcp/sweep_quiet_test.go:TestSweepPassErrorIsLoggedOncePerProcess",
	}},
	// E21 T3 — the mention. The claim is "only OUR renderer mints a token", so the list carries the mint, the
	// two refusals (a model-chosen id, a model-written token) and the migration that makes the identity
	// durable — plus the E20 AST guard, because a renderer that started minting actionable elements while
	// minting mentions would break the older rule this one is built beside.
	"TLM-003": {"unit", []string{
		"adapters/integrations/slack/mention_test.go:TestAMentionTheModelWroteIsStillNeutralized",
		"adapters/integrations/slack/mention_test.go:TestMentionMintsExactlyOneIDAndItIsTheDeliveryRows",
		"adapters/integrations/slack/mention_test.go:TestTheModelCannotChooseWhoIsMentioned",
		"adapters/integrations/slack/mention_test.go:TestAnEmptyRequesterSkipsTheMentionAndKeepsTheWords",
		"adapters/integrations/slack/mention_test.go:TestAPublicationDisplayCannotBroadcast",
		// TestStatusCannotBroadcast WITHDRAWN 2026-08-05 — dfceb769 deleted SetStatus itself as dead
		// HTTP-transport code (Socket Mode replaced the old bridge; nothing calls
		// assistant.threads.setStatus in production), and its test with it. See the matching WITHDRAWN
		// note in tests/uat/cases/TLM-003/case.yaml.
		"adapters/integrations/slack/mention_test.go:TestAMentionRendersAsAnOrdinarySection",
		"adapters/integrations/slack/blocks_test.go:TestNoFileButInteractionsMintsAnActionableElement",
		// TestMigration43SlackRequester WITHDRAWN 2026-08-07 — 000009_drop_slack_cutover_tables dropped the
		// two tables whose requester_user_id columns it pinned. The identity lives in apps/slack-bot now.
		// See the matching WITHDRAWN note in tests/uat/cases/TLM-003/case.yaml.
	}},
	// E21 T4 — the tool surface. Four of these drive the REAL Orchestrator, which is the gap §3.6 D3 measured:
	// every prior MCP proof stopped at the broker, and the only engine-level evidence was behind a live tag.
	"TLM-004": {"component-real", []string{
		"apps/control-plane/internal/extensions/tool_class_test.go:TestOnlyControlPlaneOutputIsTrusted",
		"apps/control-plane/internal/extensions/tool_class_test.go:TestTheNoticeSurvivesAnEmptyDescription",
		"apps/control-plane/internal/execution/tool_surface_component_test.go:TestTheRealOrchestratorAdvertisesARegistryToolToTheProvider",
		"apps/control-plane/internal/execution/tool_surface_component_test.go:TestAnExternalToolIsAdvertisedAsUntrustedToTheModel",
		"apps/control-plane/internal/execution/tool_surface_component_test.go:TestARunReachesNoMCPServerItsRiderDoesNotName",
		"apps/control-plane/internal/execution/tool_surface_component_test.go:TestARegistryToolDispatchesThroughTheRealOrchestratorAndTheResultReachesTheEngine",
		// `1e5fc63e` deleted up_tools_test.go with the SLACK_AGENT_TOOLS behaviour it proved; four of its
		// five proofs are withdrawn and the fifth follows the behaviour to its canonical replacement.
		// Recorded in TLM-004/case.yaml.
		"cmd/cli/internal/stack/up_default_tools_test.go:TestBringUpGrantsTheCanonicalDefaultSet",
		"apps/control-plane/internal/execution/config_test.go:TestResolveGrantsToolSetsOnANullProjectBaseline",
		// The dead-tool guard: palai.slack.search was absent from `palai up`'s default list when T5
		// shipped, so the search tool was mounted, tested and unreachable. cmd/cli cannot import the tool
		// package, so nothing compared the two sides until this did. The comparison SURVIVES — it now
		// reads packages/toolset rather than a Slack-named literal, hence the renames. Its third leg
		// (TestTheSearchToolIsInTheDefaultSet) is withdrawn: nothing grants palai.slack.search today.
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestEveryDefaultToolResolves",
		"apps/control-plane/internal/execution/tools/default_set_test.go:TestNoDefaultToolPublishes",
	}},
	// E21 T5 — the workspace search. The last two entries are E21 T7's own: the DECLARATION that the tool's
	// output may not be persisted, and the journey that sweeps what the run actually wrote down. They are here
	// because the case's crown sentence — "no result reaches a table" — was FALSE until this gate ran it: every
	// tool result is committed to the ledger before delivery, so the results sat verbatim in tool_calls.result.
	// A case whose proof list stopped at the unit tier would still be certifying that sentence today.
	"TLM-005": {"component-real", []string{
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestTheSearchToolIsNotOfferedToARunWithNoAuthority",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestAnUnrelatedToolNameFallsThroughToTheRegistry",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestTheModelCannotWidenTheSearchScope",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestAnExhaustedSearchBudgetSaysSoRatherThanReturningNothing",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestARegrantDoesNotRefillTheBudget",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestTheActionTokenNeverReachesTheModel",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestResultsReachTheModelLabelledUntrusted",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestASearchResultCarriesNothingActionable",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestReleaseEndsTheRunsSearchAuthority",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestThePacerIsWorkspaceWideAndPacesTheSecondCall",
		"apps/control-plane/internal/execution/tools/slack_search_test.go:TestTheSearchToolsOutputIsDeclaredUnretained",
		"apps/control-plane/internal/execution/tools_memory_journey_component_test.go:TestToolsMemoryJourney",
	}},
}

// caseAssertion is the bundle's per-case sentence, DERIVED from this catalog so a bundle assertion can never
// describe a proof list the case does not declare.
func caseAssertion(t *testing.T, id string) string {
	t.Helper()
	entry, ok := expectedToolsMemoryCatalog[id]
	if !ok {
		t.Fatalf("%s has no catalog entry to derive its bundle assertion from", id)
	}
	return fmt.Sprintf("%s: proven by %d in-tree proof(s) at the %s tier — %s",
		id, len(entry.proofs), entry.class, strings.Join(entry.proofs, "; "))
}

// TestToolsMemoryCatalogMaterialized is the E21 catalog gate. It rides make verify (no Docker, no
// credential), so a forgotten case, a vanished proof leg, or a case text that hides which seam produced its
// evidence fails fast.
func TestToolsMemoryCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedToolsMemoryCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c memoryCase
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
		if !strings.Contains(c.Input, "§6") && !strings.Contains(c.Input, "leg 1") {
			t.Errorf("%s: the case text names no §6 operator leg — every case must say what real-infrastructure leg it does NOT cover", id)
		}
		assertMemoryProofs(t, root, id, want.class, want.proofs, c.Proof)
	}

	// The E21 side of the split ownership: every id this package claims must actually have a directory. The
	// E17 gate covers the other direction (no dir under a guarded prefix escapes every catalog).
	for _, id := range uat.ToolsMemoryCaseIDs {
		if _, ok := expectedToolsMemoryCatalog[id]; !ok {
			t.Errorf("%s is in uat.ToolsMemoryCaseIDs but this gate resolves no proofs for it — the canonical list and the catalog must be the same set", id)
		}
		if _, err := os.Stat(filepath.Join(casesDir, id, "case.yaml")); err != nil {
			t.Errorf("%s is claimed by this epic but has no case.yaml: %v", id, err)
		}
	}
	for id := range expectedToolsMemoryCatalog {
		if !slices.Contains(uat.ToolsMemoryCaseIDs, id) {
			t.Errorf("%s is cataloged here but is not in uat.ToolsMemoryCaseIDs, so the E17 orphan guard would report it as an escapee", id)
		}
	}
}

// TestEveryTLMDirectoryIsOwned is the E21 half of the orphan rule, asserted HERE as well as in the E17 gate.
// Two gates checking it is not redundancy: the E17 sweep walks the directory and defers to this epic's list,
// so if somebody deleted the `TLM-` entry from extensionIDPrefixes the sweep would go quiet and stay green.
// This one walks the same directory and cannot be silenced by that edit.
func TestEveryTLMDirectoryIsOwned(t *testing.T) {
	casesDir := filepath.Join(repoRoot(t), "tests", "uat", "cases")
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	seen := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "TLM-") {
			continue
		}
		seen++
		if _, ok := expectedToolsMemoryCatalog[e.Name()]; !ok {
			t.Errorf("%s is a TLM- case dir that NO catalog resolves proofs for — it escapes proof resolution entirely", e.Name())
		}
	}
	if seen != len(uat.ToolsMemoryCaseIDs) {
		t.Errorf("the tree holds %d TLM- case dir(s) but this epic claims %d — one of the two is stale", seen, len(uat.ToolsMemoryCaseIDs))
	}
}

// assertMemoryProofs resolves every referenced proof in the tree and refuses a tier overclaim: a case
// declaring component-real must reference at least one proof carrying the `component` build tag, or it is
// claiming a real backing service it never touched.
func assertMemoryProofs(t *testing.T, root, id, class string, want, got []string) {
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
