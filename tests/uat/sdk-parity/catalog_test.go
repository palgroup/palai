package sdkparity

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// spCatalogCase is the case.yaml catalog record for an E16 SDK-parity case — the same shape as the self-host
// shCase / managed-cloud mciCase: a structured `proof:` list so the gate can assert the referenced proof
// genuinely exists in the tree at the declared build tier. A case may not claim a half T1-T7 did not prove.
//
// ponytail: this file is the SIXTH copy-adaptation of tests/uat/automation/catalog_test.go (autoCase/
// validProofClasses/honestNamePattern/buildClass/assertProofsMatch). Exporting those to a shared tests/uat
// package is a separate refactor, not this task.
type spCatalogCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary an SDK-parity case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// sdkParityIDPrefixes are the case-id families E16 T8 owns. No API-/MOD- case dir existed before this gate, so
// this gate is the first materializer of both prefixes; a future epic that adds an API-/MOD- case extends
// expectedSDKParityCatalog (the OPS-/DR- precedent in self-host).
var sdkParityIDPrefixes = []string{"API-", "MOD-"}

// expectedSDKParityCatalog is the E16 SDK-parity UAT catalog: every case ID this epic materializes (plan §T8 —
// API-012..015 + MOD-001..012), mapped to the proof class its case.yaml must declare and the in-tree proof(s)
// that already prove it (T1-T7). The declared class is the BUILD-TAG TIER of the referenced proof — MECHANICALLY
// enforced by assertProofsMatch — so no case can overclaim its tier.
//
// HONEST NAMING (spec §10.2): the plan §T8 journey aspires the live tier to live-provider (make uat-sdk-parity
// PROVIDER=provider-one), but the LOAD-BEARING in-tree proof that rides this gate is the
// deterministic/component/unit half — the live four-client + gateway-off + real-provider journey is the script
// tier (scripts/uat/sdk-parity), the same split E11-E15's catalogs used. So a case declares the tier of the
// proof this gate can actually read: the SDK-conformance + model-conformance halves are unit (they ride make
// verify), MOD-011 budget settlement is component-real (a real Postgres ledger).
var expectedSDKParityCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// API-012..015 — the SDK cross-language + retry + signature + 410-tombstone surface (T1/T2/T3/T4).
	"API-012": {"unit", []string{
		"tests/conformance/sdk/harness_test.go:TestCorpusReferenceEquality",
		"tests/conformance/sdk/harness_test.go:TestCorpusTypeScriptRunnerEquality",
		"tests/conformance/sdk/harness_test.go:TestCorpusPythonRunnerEquality",
		"tests/conformance/sdk/go_runner_test.go:TestCorpusGoRunnerEquality",
		"tests/conformance/sdk/harness_test.go:TestHarnessFailsOnDivergence",
	}},
	"API-013": {"unit", []string{
		"sdks/go/client_test.go:TestRetryReusesSameIdempotencyKey",
		"sdks/go/client_test.go:TestNonIdempotentPostFailsClosed",
	}},
	"API-014": {"unit", []string{
		"sdks/go/webhook_test.go:TestVerifyWebhook",
		"sdks/go/webhook_test.go:TestVerifyWebhookRotationOverlap",
	}},
	"API-015": {"unit", []string{
		"apps/control-plane/api/responses_admission_test.go:TestAdmitPurgedReplayRenders410Tombstone",
	}},
	// MOD-001..012 — the two-provider-family runtime conformance (T5/T6).
	"MOD-001": {"unit", []string{
		"tests/conformance/models/adapters_canonical_test.go:TestAdaptersConvergeOnCanonicalToolResult",
	}},
	"MOD-002": {"unit", []string{
		"tests/conformance/models/capability_filter_conformance_test.go:TestCapabilityHardFilterRejectsUnsupportedBeforeAdmission",
	}},
	"MOD-003": {"unit", []string{
		"tests/conformance/models/adapters_canonical_test.go:TestAdaptersConvergeOnCanonicalToolResult",
		"tests/conformance/models/resilience_conformance_test.go:TestChainRoutesAroundMisconfiguredTarget",
	}},
	"MOD-004": {"unit", []string{
		"tests/conformance/models/capability_filter_conformance_test.go:TestCapabilityHardFilterRejectsUnsupportedBeforeAdmission",
	}},
	"MOD-005": {"unit", []string{
		"tests/conformance/models/resilience_conformance_test.go:TestChainFailsOverAfterPartialWithHonestAttemptCount",
	}},
	"MOD-006": {"unit", []string{
		"tests/conformance/models/lifecycle_conformance_test.go:TestWireAdaptersSurfaceTruncatedStreamAsVisiblePartial",
	}},
	"MOD-007": {"unit", []string{
		"tests/conformance/models/lifecycle_conformance_test.go:TestFakeIdempotentReplayServesStoredEffectWithoutReExecuting",
	}},
	"MOD-008": {"unit", []string{
		"tests/conformance/models/resilience_conformance_test.go:TestChainFailsOverAfterPartialWithHonestAttemptCount",
	}},
	"MOD-009": {"unit", []string{
		"tests/conformance/models/lifecycle_conformance_test.go:TestAdaptersHonorMidStreamCancel",
		"tests/conformance/models/resilience_conformance_test.go:TestChainCancelStopsTheChainWithoutFailingOver",
	}},
	"MOD-010": {"unit", []string{
		"tests/conformance/models/usage_cache_conformance_test.go:TestAdaptersFoldProviderCacheCountersIntoCanonicalUsage",
	}},
	"MOD-011": {"component-real", []string{
		"tests/component/postgres/usage_ledger_test.go:TestCommitModelResultSettlesUsageExactlyOnce",
		"tests/component/postgres/usage_ledger_test.go:TestAdmissionRejectsWhenTheDurableBudgetIsExhausted",
	}},
	"MOD-012": {"unit", []string{
		"tests/conformance/models/resilience_conformance_test.go:TestChainCallerInvalidTripsNothingAndFailsOverNowhere",
		"tests/conformance/models/resilience_conformance_test.go:TestChainUpstreamFailuresTripCircuitThenShed",
	}},
}

// TestSDKParityCatalogMaterialized is the E16 SDK-parity catalog gate: every proven half from T1-T7 has a
// case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at an in-tree
// proof that actually exists. It rides make verify (no Docker), so a forgotten case, an overclaimed class, or a
// case referencing a proof not in the tree fails fast.
func TestSDKParityCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedSDKParityCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c spCatalogCase
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

	// Orphan guard: every API-/MOD- case dir must be in the map, so a stray dir cannot escape proof resolution.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range sdkParityIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedSDKParityCatalog[e.Name()]; !ok {
					t.Errorf("%s: an API-/MOD-family case dir is not in expectedSDKParityCatalog (add it, or it escapes proof resolution)", e.Name())
				}
				break
			}
		}
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no build tag
// runs in make verify / test-unit, so it is the "unit" tier.
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
// "path/to/file.go:FuncName" resolves to a file that declares that func AND whose //go:build tier equals the
// declared class — so the declared class is mechanically the tier that runs the proof, not just prose.
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
