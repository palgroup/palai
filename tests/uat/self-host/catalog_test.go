// Package selfhost holds the E14 self-host UAT catalog gate + the self-host-0.1.0 evidence-release
// verification. Both are Docker-free pure checks, so they ride `make verify` (no credential, no stack): the
// catalog gate asserts every case this slice owns (plan §T7 — OPS-002 + DR-002 + DR-004..006) is MATERIALIZED
// — present, honestly named, declaring a valid proof class, and pointing at an in-tree proof whose //go:build
// tier equals the declared class — and the evidence gate (evidence_test.go) asserts the committed
// self-host-0.1.0 bundle verifies clean through the shared verifier (0 findings, 0 secret findings) with every
// self-host rule active.
//
// ponytail: this file is the FIFTH copy-adaptation of tests/uat/automation/catalog_test.go (autoCase/
// validProofClasses/honestNamePattern/buildClass/assertProofsMatch/repoRoot). Exporting those helpers to a
// shared tests/uat package is a separate refactor, not this task.
package selfhost

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// shCase is the case.yaml catalog record for a self-host case — identical shape to the managed-cloud mciCase:
// a structured `proof:` list so the gate can assert the referenced proof genuinely exists in the tree at the
// declared tier. A case may not claim a half T1-T6 did not already prove.
type shCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary a self-host case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// selfHostIDPrefixes are the case-id families whose orphan guard E14 T7 owns. E15 (T2 + T6) extended this
// catalog with OPS-003..008 + DR-001 (the upgrade/DR/air-gap/helm halves), so every materialized OPS-/DR- dir
// must be in expectedSelfHostCatalog; OPS-001 stays reserved for E07 and DR-003 for the SaaS plan. A future
// epic that materializes another extends this catalog (the managed-cloud MCI- precedent).
var selfHostIDPrefixes = []string{"OPS-", "DR-"}

// expectedSelfHostCatalog is the E14 self-host UAT catalog: every case ID this slice materializes (plan §T7),
// mapped to the proof class its case.yaml must declare and the in-tree proof(s) that already prove it (T1-T6).
// The declared class is the BUILD-TAG TIER of the referenced proof — MECHANICALLY enforced by assertProofsMatch
// reading each proof file's //go:build tag — so a reader can run exactly that tier to see the proof and no case
// can overclaim its tier. A missing dir, a drifted class, a tag/class mismatch, or a proof reference that does
// not resolve fails the gate.
//
// HONEST NAMING (spec §10.2, the gate writes the mechanical tier): the plan §T7 journey aspires the live tier
// to live-provider (make uat-self-host PROVIDER=provider-one), but the LOAD-BEARING in-tree proof that rides
// this gate is the deterministic/component/unit half — the live production-compose journey is the script tier
// (scripts/uat/self-host), the same split E11/E12/E13's catalogs used. So a case declares the tier of the proof
// this gate can actually read: OPS-002 and DR-002/004/005 are unit (the T1 posture guard, T3 config-validate +
// doctor v2, and the T4 backup archive/round-trip/empty-target proofs — tag-less proofs that ride make verify),
// DR-006 is component-real (the T4 secret canary against a real Postgres with two master keys).
var expectedSelfHostCatalog = map[string]struct {
	class  string
	proofs []string
}{
	"OPS-002": {"unit", []string{
		"deploy/compose/production_guard_test.go:TestProductionGuardAdmitsRealKeys",
		"deploy/compose/production_guard_test.go:TestProductionGuardRefusesDevMasterKey",
		"cmd/cli/internal/stack/configvalidate_test.go:TestConfigValidatePassesOnGoodConfig",
		"cmd/cli/internal/stack/doctor_v2_test.go:TestDoctorReusesMetricsQueries",
	}},
	"DR-002": {"unit", []string{
		"cmd/cli/internal/stack/install_backup_test.go:TestBackupArchiveRoundTrip",
	}},
	"DR-004": {"unit", []string{
		"cmd/cli/internal/stack/install_backup_test.go:TestBackupArchiveRejectsCorruptedMember",
		"cmd/cli/internal/stack/install_backup_test.go:TestObjectChecksumsPerFile",
	}},
	"DR-005": {"unit", []string{
		"cmd/cli/internal/stack/install_backup_test.go:TestBuildExcessQuery",
	}},
	"DR-006": {"component-real", []string{
		"apps/control-plane/internal/identity/install_backup_canary_component_test.go:TestInstallRestoreSecretCanaryTwoMasterKeys",
	}},
	// E15 T2 extends this E14-owned OPS-/DR- catalog (the plan reserved OPS-005/007/008 for the upgrade
	// epic). All three ride untagged (unit-tier) proofs that run in make verify — the CLI compat/manifest
	// units, the version-window unit, and the gateway drain/skew wire tests; the live two-build upgrade
	// drill (make upgrade-drill) is the journey tier the case inputs name.
	"OPS-005": {"unit", []string{
		"cmd/cli/internal/stack/upgrade_test.go:TestVerifyUpgradeCompat",
		"cmd/cli/internal/stack/upgrade_test.go:TestLoadReleaseManifestRejectsIncomplete",
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayDrainWaitsForInFlightLease",
	}},
	"OPS-007": {"unit", []string{
		"cmd/cli/internal/stack/upgrade_test.go:TestVerifyUpgradeCompat",
	}},
	"OPS-008": {"unit", []string{
		"packages/version/version_test.go:TestSupportedWindow",
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayRejectsUnsupportedRunnerSkew",
	}},
	// E15 T6 (the SH-2 RC EXIT gate, plan §T6) extends this OPS-/DR- catalog with the remaining
	// upgrade/DR/air-gap/helm halves. Each rides an in-tree proof at its real build tier: OPS-003 the
	// render/policy asserts, OPS-004 the signed OFFLINE air-gap verify + tamper rejection, and DR-001 the DR
	// measurement recompute (all unit — they ride make verify); OPS-006 the migration-journal interruption/
	// resume + preflight against a REAL Postgres (component-real); SAN-011 the gateway cordon/drain/revoke wire
	// tests (unit). The live two-build upgrade + measured DR + offline air-gap + kind smoke is the make uat-sh2
	// journey tier the case inputs name — this gate reads the deterministic half only (the E11-E14 split).
	"OPS-003": {"unit", []string{
		"tests/uat/kubernetes/render_assert_test.go:TestNoClusterRole",
		"tests/uat/kubernetes/render_assert_test.go:TestControlPlaneSecurityContextRestricted",
		"tests/uat/kubernetes/render_assert_test.go:TestNetworkPolicyDefaultDeny",
		"tests/uat/kubernetes/render_assert_test.go:TestMigrationJobIsPreInstallHook",
		"tests/uat/kubernetes/render_assert_test.go:TestPodDisruptionBudgetPresent",
		"tests/uat/kubernetes/render_assert_test.go:TestNoInClusterDatabase",
	}},
	"OPS-004": {"unit", []string{
		"deploy/airgap/airgap_test.go:TestBundleBuildsAndVerifies",
		"deploy/airgap/airgap_test.go:TestVerifyFailsOnTamperedComponent",
		"deploy/airgap/airgap_test.go:TestVerifyRejectsWrongKey",
	}},
	"OPS-006": {"component-real", []string{
		"tests/component/postgres/migration_journal_test.go:TestMigrationInterruptionResumes",
		"tests/component/postgres/migration_journal_test.go:TestMigrationJournalRecordsChainHead",
		"tests/component/postgres/migration_journal_test.go:TestMigrationPreflightRejectsNewerDatabase",
	}},
	"DR-001": {"unit", []string{
		"tests/uat/dr/report_test.go:TestVerifyRecomputesAndCatchesFabrication",
	}},
	"SAN-011": {"unit", []string{
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayDialRefusesWhenCordoned",
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayDrainWaitsForInFlightLease",
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayRevokeRefusesConnectAndDial",
		"apps/control-plane/internal/execution/runner_gateway_test.go:TestGatewayRevokeDropsInFlightSessionFrames",
	}},
}

// TestSelfHostCatalogMaterialized is the E14 self-host-catalog gate: every proven half from T1-T6 has a
// case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at an in-tree
// proof that actually exists. It rides make verify (no Docker), so a forgotten case, an overclaimed class, or a
// case that references a proof not in the tree fails fast.
func TestSelfHostCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedSelfHostCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c shCase
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

	// Orphan guard: every OPS-/DR- case dir must be in the map, so a stray dir with a self-host prefix cannot
	// escape proof resolution.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range selfHostIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedSelfHostCatalog[e.Name()]; !ok {
					t.Errorf("%s: an OPS-/DR-family case dir is not in expectedSelfHostCatalog (add it, or it escapes proof resolution)", e.Name())
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
