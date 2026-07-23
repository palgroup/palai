// Package managedcloud holds the E13 managed-cloud UAT catalog gate + the managed-cloud-0.1.0 evidence-
// release verification. Both are Docker-free pure checks, so they ride `make verify` (no credential, no
// stack): the catalog gate asserts every MCI case this slice owns is MATERIALIZED — present, honestly named,
// declaring a valid proof class, and pointing at an in-tree proof whose //go:build tier equals the declared
// class — and the evidence gate (evidence_test.go) asserts the committed managed-cloud-0.1.0 bundle verifies
// clean through the shared verifier (0 findings, 0 secret findings) with every managed-cloud rule active.
//
// ponytail: this file is the FOURTH copy-adaptation of tests/uat/automation/catalog_test.go (mciCase/
// validProofClasses/honestNamePattern/buildClass/assertProofsMatch/repoRoot). Exporting those helpers to a
// shared tests/uat package is a separate refactor, not this task.
package managedcloud

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// mciCase is the case.yaml catalog record for a managed-cloud case — identical shape to the automation
// autoCase / extensibility extCase: a structured `proof:` list so the gate can assert the referenced proof
// genuinely exists in the tree at the declared tier. A case may not claim a half T1-T10 did not already prove.
type mciCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary a managed-cloud case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// managedCloudIDPrefixes are the case-id families whose orphan guard E13 T11 owns. MCI- is E13-exclusive.
var managedCloudIDPrefixes = []string{"MCI-"}

// expectedManagedCloudCatalog is the E13 managed-cloud UAT catalog: every case ID this slice materializes
// (plan §T11, MCI-001..008), mapped to the proof class its case.yaml must declare and the in-tree proof(s)
// that already prove it (T1-T10). The declared class is the BUILD-TAG TIER of the referenced proof —
// MECHANICALLY enforced by assertProofsMatch reading each proof file's //go:build tag — so a reader can run
// exactly that tier to see the proof and no case can overclaim its tier. A missing dir, a drifted class, a
// tag/class mismatch, or a proof reference that does not resolve fails the gate.
//
// HONEST NAMING (spec §10.2, the gate writes the mechanical tier): the plan §T11 journey aspires the live
// tier to live-provider (make uat-managed-cloud PROVIDER=provider-one), but the LOAD-BEARING in-tree proof
// that rides this gate is the deterministic/component/unit half — the live journey is the script tier
// (scripts/uat/managed-cloud), the same split E11/E12's catalogs used. So a case declares the tier of the
// proof this gate can actually read: MCI-001/002/004/006/007 are component-real, MCI-003/008 are
// e2e-deterministic, MCI-005 is unit (tag-less proofs that ride make verify — the AUT-012 precedent). MCI-008
// references the SERVER steer proof (Go, e2e); the @palai/sdk TypeScript client parity is proven by its own
// vitest suite (named in the case input as the ceiling, not a Go-referenceable proof).
var expectedManagedCloudCatalog = map[string]struct {
	class  string
	proofs []string
}{
	"MCI-001": {"component-real", []string{
		"apps/control-plane/internal/identity/provisioning_component_test.go:TestProvisionSecondTenantViaAPI",
		"apps/control-plane/internal/identity/provisioning_component_test.go:TestConfigPolicyResolverReachable",
	}},
	"MCI-002": {"component-real", []string{
		"apps/control-plane/internal/identity/secrets_component_test.go:TestSecretRefWriteResolveRotate",
		"apps/control-plane/internal/identity/secrets_component_test.go:TestSecretRefCrossOrgResolveDenied",
	}},
	"MCI-003": {"e2e-deterministic", []string{
		"apps/control-plane/e2e/responses/list_test.go:TestListResponsesRejectsCrossTenantCursor",
	}},
	"MCI-004": {"component-real", []string{
		"apps/control-plane/internal/artifacts/reader_component_test.go:TestReaderMetadataAndContent",
		"apps/control-plane/internal/artifacts/reader_component_test.go:TestReaderCrossTenantIsMiss",
	}},
	"MCI-005": {"unit", []string{
		"apps/control-plane/api/middleware/ratelimit_test.go:TestRateLimitMiddlewareEmitsProblem",
		"apps/control-plane/api/responses_admission_test.go:TestAdmitConcurrencyLimitedRenders429",
	}},
	"MCI-006": {"component-real", []string{
		"apps/control-plane/internal/execution/model_route_component_test.go:TestProjectModelRouteRoutesPerProject",
		"tests/component/postgres/model_route_test.go:TestProjectModelRouteResolvesPublishedRevisionOnly",
	}},
	"MCI-007": {"component-real", []string{
		"apps/control-plane/internal/execution/repository_binding_component_test.go:TestBindingConnectionRefClonesUnderTenantCredential",
	}},
	"MCI-008": {"e2e-deterministic", []string{
		"apps/control-plane/e2e/responses/commands_test.go:TestSteerAppliesAtNextLoopBoundaryWithSequence",
	}},
}

// TestManagedCloudCatalogMaterialized is the E13 managed-cloud-catalog gate: every proven half from T1-T10
// has a case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at
// an in-tree proof that actually exists. It rides make verify (no Docker), so a forgotten case, an
// overclaimed class, or a case that references a proof not in the tree fails fast.
func TestManagedCloudCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedManagedCloudCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c mciCase
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

	// Orphan guard: every MCI- case dir must be in the map. MCI- is E13 T11-exclusive, so a stray MCI- dir
	// cannot escape proof resolution.
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range managedCloudIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedManagedCloudCatalog[e.Name()]; !ok {
					t.Errorf("%s: an MCI-family case dir is not in expectedManagedCloudCatalog (add it, or it escapes proof resolution)", e.Name())
				}
				break
			}
		}
	}
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. A file with no build
// tag runs in make verify / test-unit, so it is the "unit" tier.
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
