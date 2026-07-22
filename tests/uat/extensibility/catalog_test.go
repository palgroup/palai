// Package extensibility holds the E12 extensibility UAT catalog gate + the extensibility-0.1.0
// evidence-release verification. Both are Docker-free pure checks, so they ride `make verify` (no
// credential, no stack): the catalog gate asserts every extensibility case this slice owns is
// MATERIALIZED — present, honestly named, declaring a valid proof class, and pointing at an in-tree
// proof whose //go:build tier equals the declared class — and the evidence gate (evidence_test.go)
// asserts the committed extensibility-0.1.0 bundle verifies clean through the shared verifier (0
// findings, 0 secret findings) with every extension rule active.
//
// ponytail: this file is the THIRD copy-adaptation of tests/uat/automation/catalog_test.go (extCase/
// validProofClasses/honestNamePattern/buildClass/assertProofsMatch/repoRoot). Exporting those helpers to
// a shared tests/uat package is a separate refactor, not this task.
package extensibility

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// extCase is the case.yaml catalog record for an extensibility case — identical shape to the automation
// autoCase and the recovery recCase: a structured `proof:` list so the gate can assert the referenced
// proof genuinely exists in the tree at the declared tier. A case may not claim a half that is not already
// proven by T1-T9.
type extCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary an extensibility case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// extensibilityIDPrefixes are the case-id families whose orphan guard E12 owns. EXT- is E12-exclusive.
// TOL- was owned EXCLUSIVELY by the recovery catalog (its recoveryIDPrefixes) before this slice; E12 T10
// TRANSFERS TOL- orphan-guard ownership here — recovery drops "TOL-" from its prefix sweep so this gate is
// the single owner of TOL- orphan resolution. recoveryOwnedTOL below carries the recovery-only TOL cases
// this gate must not falsely flag, so no TOL- dir is left uncovered by either gate.
var extensibilityIDPrefixes = []string{"EXT-", "TOL-"}

// recoveryOwnedTOL are the TOL- cases the RECOVERY catalog (tests/uat/recovery/catalog_test.go) owns and
// validates in its own map — the pure ledger/replay half (spec §26.6-26.7, E10). They are NOT E12 cases,
// so the extensibility orphan guard allowlists them (they are covered by the recovery gate's map loop).
// TOL-016/017 are DELIBERATELY absent: they are shared cases (E10 ledger/fence half + E12 signed-transport
// half), so they ARE map keys here (with the combined proof list) AND in the recovery map.
var recoveryOwnedTOL = map[string]bool{
	"TOL-001": true, "TOL-002": true, "TOL-003": true, "TOL-004": true,
}

// expectedExtensibilityCatalog is the E12 extensibility UAT catalog: every case ID this slice materializes
// (spec §28 acceptance contract EXT/TOL, §3 table), mapped to the proof class its case.yaml must declare
// and the in-tree proof(s) that already prove it (T1-T9). The declared class is the BUILD-TAG TIER of the
// referenced proof — MECHANICALLY enforced by assertProofsMatch reading each proof file's //go:build tag —
// so a reader can run exactly that tier to see the proof and no case can overclaim its tier. A missing dir,
// a drifted class, a tag/class mismatch, or a proof reference that does not resolve fails the gate.
//
// HONEST NAMING (spec §10.2, the gate writes the mechanical tier): the plan §3 table aspires some cases to
// live-provider (EXT-001/EXT-002/EXT-004/TOL-010), but the LOAD-BEARING in-tree proof that rides the gate
// is the deterministic/component/unit half — the live spontaneous half is the script tier (scripts/uat/
// extensibility), the same split E11's catalog used (its live smokes are not catalog rows). So a case
// declares the tier of the proof this gate can actually read: EXT-001/002 are e2e-deterministic (the
// advertising e2e), EXT-003/006 + TOL-008/017 are component-real, EXT-004 + TOL-009/010/011/012/016/018 are
// unit (tag-less proofs that ride make verify — the AUT-012 precedent), EXT-005 is fault-live (a real
// worker-down kill). TOL-016/017 carry the COMBINED proof list (E10 half + E12 signed half) — both halves
// happen to share a tier (016 both unit, 017 both component), so the shared case.yaml satisfies this gate
// AND the recovery gate without weakening either.
var expectedExtensibilityCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// EXT — advertising ceiling resolution + registry + web-research + crash isolation (spec §28, §3 table).
	"EXT-001": {"e2e-deterministic", []string{
		"apps/control-plane/e2e/responses/tool_advertising_test.go:TestDispatchModelAdvertisesEffectiveToolSchemas",
	}},
	"EXT-002": {"e2e-deterministic", []string{
		"apps/control-plane/e2e/responses/tool_advertising_test.go:TestToolOutsideEffectiveSetNeverAdvertised",
	}},
	"EXT-003": {"component-real", []string{
		"apps/control-plane/internal/extensions/registry_component_test.go:TestToolRevisionImmutableAndDigestPinned",
		"apps/control-plane/internal/extensions/registry_component_test.go:TestCanonicalNamespaceCollisionRejected",
		"apps/control-plane/internal/extensions/registry_component_test.go:TestModelVisibleShortNameDeterministicCollisionChecked",
	}},
	"EXT-004": {"unit", []string{
		"apps/control-plane/internal/execution/tools/research_test.go:TestResearchFetchProducesCitations",
		"apps/control-plane/internal/execution/tools/research_test.go:TestResearchDeniesPrivateAndMetadataTargetsAfterResolveAndRedirect",
		"apps/control-plane/internal/execution/config_research_test.go:TestResearchRequiresNetworkCapabilityAndNeverPublishCapability",
	}},
	"EXT-005": {"fault-live", []string{
		"apps/control-plane/internal/extensions/hook_worker_down_fault_test.go:TestHookWorkerDownFailsClosedTripsBreakerControlPlaneUp",
	}},
	"EXT-006": {"component-real", []string{
		"apps/control-plane/internal/extensions/mcp_dispatch_component_test.go:TestAnnotationChangeRequiresNewRevisionAndReapproval",
	}},

	// TOL — tool-runtime extensibility halves (spec §28.13-28.25). TOL-016/017 carry BOTH the E10 ledger/
	// fence half and the E12 signed-transport half (shared-case discipline); the two halves share a tier.
	"TOL-008": {"component-real", []string{
		"adapters/integrations/mcp/stdio_component_test.go:TestMCPStdioDiscoveryCallProgressCancel",
		"apps/control-plane/internal/extensions/mcp_dispatch_component_test.go:TestDiscoveredToolsNamespacedByConnectionNoCollision",
		"apps/control-plane/internal/extensions/mcp_dispatch_component_test.go:TestMCPCallFlowsThroughStandardDispatch",
	}},
	"TOL-009": {"unit", []string{
		"tests/security/extensions/mcp_test.go:TestMCPServerCannotReplayTokenToAnotherUpstream",
		"tests/security/extensions/mcp_test.go:TestMCPServerCannotCallPlatformWithReceivedCredentials",
		"adapters/integrations/mcp/auth_test.go:TestUpstreamTokenNeverForwardedToMCPServer",
		"adapters/integrations/mcp/auth_test.go:TestAudienceMismatchDenied",
	}},
	"TOL-010": {"unit", []string{
		"adapters/integrations/mcp/sampling_test.go:TestSamplingDeniedByDefault",
		"apps/control-plane/internal/execution/mcp_sampling_test.go:TestSamplingEnabledRoutesBrokeredBudgetedVisibleStep",
	}},
	"TOL-011": {"unit", []string{
		"apps/control-plane/internal/extensions/skill_security_test.go:TestSkillSecurityMaliciousArchivesHardRejected",
		"apps/control-plane/internal/extensions/quarantine_test.go:TestSkillArchiveTraversalSymlinkBombRejected",
		"apps/control-plane/internal/execution/config_test.go:TestSkillInstructionsGrantNoCapability",
	}},
	"TOL-012": {"unit", []string{
		"apps/control-plane/internal/extensions/hooks_test.go:TestPolicyHookTimeoutFailsClosed",
		"apps/control-plane/internal/extensions/hooks_test.go:TestObserverHookCrashFailsOpenRunUnaffected",
	}},
	"TOL-016": {"unit", []string{
		"packages/tool-broker/broker_test.go:TestDuplicateToolCallIdSingleExecution",
		"adapters/tools/http/executor_test.go:TestRemoteDuplicateRetrySameToolCallIdSingleExecution",
	}},
	"TOL-017": {"component-real", []string{
		"apps/control-plane/internal/execution/tool_ledger_component_test.go:TestLateCallbackAfterFenceAdvanceDenied",
		"apps/control-plane/internal/execution/remote_prober_component_test.go:TestLateCallbackAfterDeadlineEntersReconciliationNotSilentCommit",
	}},
	"TOL-018": {"unit", []string{
		"tests/conformance/tool-sdk/conformance_test.go:TestSchemaEmitCanonicalBytes",
		"tests/conformance/tool-sdk/conformance_test.go:TestSignatureVerifyMatchesReference",
		"tests/conformance/tool-sdk/conformance_test.go:TestResultNormalizeCanonicalBytes",
	}},
}

// TestExtensibilityCatalogMaterialized is the E12 extensibility-catalog gate: every proven half from T1-T9
// has a case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at
// an in-tree proof that actually exists. It rides make verify (no Docker), so a forgotten case, an
// overclaimed class, or a case that references a proof not in the tree fails fast.
func TestExtensibilityCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedExtensibilityCatalog {
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
			t.Errorf("%s: proof_class = %q, want %q (the tier of the referenced proof — no overclaim)", id, c.ProofClass, want.class)
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
		assertProofsMatch(t, root, id, want.class, want.proofs, c.Proof)
	}

	// Orphan guard: every EXT-/TOL- case dir must be in the map OR be a recovery-owned TOL case (validated
	// by the recovery gate's map). This gate is the SINGLE owner of TOL- orphan resolution after the E12
	// split, so a stray TOL-/EXT- dir cannot escape proof resolution.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range extensibilityIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedExtensibilityCatalog[e.Name()]; !ok && !recoveryOwnedTOL[e.Name()] {
					t.Errorf("%s: an extensibility-family case dir is not in expectedExtensibilityCatalog (add it, or it escapes proof resolution)", e.Name())
				}
				break
			}
		}
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no
// build tag runs in make verify / test-unit, so it is the "unit" tier.
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
			default:
				return "unit"
			}
		}
	}
	return "unit"
}

// assertProofsMatch checks the case.yaml `proof:` list equals the catalog's expected proofs and that each
// "path/to/file.go:FuncName" resolves to a file that declares that func AND whose //go:build tier equals
// the declared class — so the declared class is mechanically the tier that runs the proof, not just prose.
func assertProofsMatch(t *testing.T, root, id, class string, want, got []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("%s: proof list = %v, want %v", id, got, want)
		return
	}
	for _, ref := range got {
		file, fn, ok := strings.Cut(ref, ":")
		if !ok {
			t.Errorf("%s: proof %q is not file.go:FuncName", id, ref)
			continue
		}
		body, err := os.ReadFile(filepath.Join(root, file))
		if err != nil {
			t.Errorf("%s: proof file %q does not exist: %v", id, file, err)
			continue
		}
		if !strings.Contains(string(body), "func "+fn+"(") {
			t.Errorf("%s: proof %q not found in %s (the case claims a proof that is not in the tree)", id, fn, file)
		}
		if got := buildClass(string(body)); got != class {
			t.Errorf("%s: proof %s is build-tier %q but the case declares proof_class %q (tier overclaim/mismatch)", id, file, got, class)
		}
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
