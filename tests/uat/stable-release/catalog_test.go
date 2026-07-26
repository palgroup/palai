// Package stablerelease is the E18 T10 FINAL cross-epic EXIT gate (plan §T10) — the `release-1.0.0-rc1`
// sign-off. Like the E15 T6 / E16 T8 / E17 T11 gates before it, the load-bearing tier is DOCKER-FREE and
// rides `make verify`: the catalog gate below, the anti-fabrication anchors (index_anchor_test.go,
// tier_anchor_test.go), the committed bundle (bundle_test.go) and the promote gate.
//
// HONEST CEILING, stated once and repeated wherever it could be forgotten: THE LOCAL CLOSURE OF THIS GATE
// IS AN RC. SH-3 Stable is the operator's attested act — `promote stable` REFUSES without an
// operator_attestation that names every §6 leg one by one — and nothing in this package declares a stable
// release, a real CI run, a transparency-log entry, a published registry artifact or a
// reference-hardware measurement.
package stablerelease

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// e18Case is the case.yaml catalog record. Same shape the E17 gate decodes.
//
// ponytail: the sixth copy-adaptation of tests/uat/automation/catalog_test.go's quartet. Hoisting it into a
// shared package is a separate refactor, not the final exit gate's job.
type e18Case struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// localSeamWords are the words a case text must use to name the seam it ACTUALLY ran on. Without one, a
// reader cannot tell a real-container proof from a fixture. Kept identical in spirit to the E17 gate's list,
// widened for the two seams this epic introduces (a signed release directory, a profiled harness run).
var localSeamWords = []string{
	"local seam", "fake", "loopback", "deterministic", "component-real", "fixture",
	"real oci", "real postgres", "real http", "offline", "profile",
}

// dockerBoundClasses: a case declaring one of these must reference at least one proof carrying the matching
// //go:build tag, so a case can never claim a real backing service or a real credential it never touched.
var dockerBoundClasses = map[string]bool{"component-real": true, "live-provider": true, "fault-live": true}

// e18IDPrefixes are the case-id families this epic owns EXCLUSIVELY, so a stray dir under one of them cannot
// escape proof resolution. "SEC-1" and not "SEC-": SEC-001..003 are E13's.
var e18IDPrefixes = []string{"SEC-1", "PER-"}

// expectedE18Catalog is the E18 UAT catalog (plan §T10, master plan Appendix A's "E18 — Supply-chain
// security" and "E18 — Performance" lines): every case this epic materializes mapped to the proof class its
// case.yaml must declare and the in-tree proof(s) that prove it. A missing dir, a drifted class, a changed
// proof list or a proof reference that does not resolve fails the gate.
var expectedE18Catalog = map[string]struct {
	class  string
	proofs []string
}{
	// SEC-101 is `unit` on the OPS-004 precedent: the identical untagged deploy/airgap/airgap_test.go tier,
	// real shipping scripts + real openssl over real bytes inside `make verify`. Its Docker-bound halves are
	// separate operator targets (release-matrix-smoke, provenance-offline-verify, PALAI_RELEASE_TOOL_IMAGE)
	// and are deliberately not folded into the class. E18 T4 shipped it declaring component-real with every
	// referenced proof untagged; this gate caught that and the case was corrected.
	"SEC-101": {"unit", []string{
		"scripts/release/release_verify_test.go:TestReleaseVerifyAcceptsACleanRelease",
		"scripts/release/release_verify_test.go:TestReleaseVerifyTamperMatrix",
		"scripts/release/release_verify_test.go:TestTamperedImageIsUnreachableUnderItsPinnedDigest",
		"scripts/release/release_verify_test.go:TestReleaseVerifyResolutionIsFailClosed",
		"scripts/release/release_verify_test.go:TestReleaseVerifyRefusesAnUnscannedRelease",
		"scripts/release/release_verify_test.go:TestReleaseVerifyMatchesAnOutOfBandRevocationList",
		"scripts/release/release_verify_test.go:TestReleaseVerifyOfflineNetworkNone",
		"scripts/release/release_verify_test.go:TestPromoteReachesTheEvidenceGate",
		"deploy/airgap/airgap_test.go:TestVerifierSwapFailsClosed",
		"deploy/airgap/airgap_test.go:TestVerifierResolutionRefusesWithNoOutOfBandSigner",
	}},
	"SEC-102": {"component-real", []string{
		"tests/uat/escape/suite_test.go:TestSandboxEscapeSuite",
		"tests/uat/escape/suite_test.go:TestEveryMaterializedSANCaseIsInTheSuite",
		"tests/uat/escape/suite_test.go:TestAnArmThatRanNothingIsNotAPass",
		"tests/uat/escape/suite_test.go:TestRedactStripsEveryCredentialShapeTheTiersEmit",
		"apps/control-plane/internal/workers/quarantine_component_test.go:TestUncertainOutcomeQuarantineIsNotSilentlyReclaimed",
	}},
	"SEC-103": {"component-real", []string{
		"packages/audit/chain_test.go:TestIntactJournalVerifiesGreen",
		"packages/audit/chain_test.go:TestDeletedRowRaisesGap",
		"packages/audit/chain_test.go:TestFlippedPayloadByteRaisesTamper",
		"packages/audit/chain_test.go:TestCheckpointSignatureFailClosed",
		"packages/audit/chain_test.go:TestALegitimateHoleIsNotAGap",
		"packages/audit/chain_test.go:TestAnOldValidlySignedCheckpointIsStale",
		"packages/audit/chain_test.go:TestARetentionPurgeIsIndistinguishableFromTamper",
		"packages/audit/chain_test.go:TestEveryRowFieldIsInTheDigest",
		"packages/audit/chain_test.go:TestPubkeyContainmentSurvivesSymlinks",
		"tests/component/postgres/audit_integrity_test.go:TestAuditIntegrityFourArms",
	}},
	"PER-001": {"component-real", []string{
		"tests/performance/per001_load_test.go:TestPER001SingleShotAndSSELoad",
		"tests/performance/harness_test.go:TestHarnessWritesNothingWithoutAProfile",
		"tests/performance/harness_test.go:TestHarnessRefusesAWhitespaceProfile",
		"tests/performance/harness_test.go:TestHarnessPercentilesRecomputeFromTheRawSamples",
		"tests/performance/harness_test.go:TestHarnessVacuousGateSurvivesIntoTheArtifact",
		"tests/performance/harness_test.go:TestHarnessThresholdOverrideIsRecorded",
	}},
	"PER-002": {"component-real", []string{
		"tests/performance/per002_sandbox_test.go:TestPER002ColdWarmSandboxPhases",
		"tests/performance/per002_sandbox_test.go:TestPER002SlowEngineFixtureFailsThePhaseBudget",
		"tests/performance/harness_test.go:TestHarnessWritesNothingWithoutAProfile",
		"tests/performance/harness_test.go:TestHarnessPercentilesRecomputeFromTheRawSamples",
	}},
	"PER-003": {"component-real", []string{
		"tests/performance/per003_soak_test.go:TestPER003LongSessionSoak",
		"tests/performance/harness_test.go:TestHarnessWritesNothingWithoutAProfile",
		"tests/performance/harness_test.go:TestHarnessPercentilesRecomputeFromTheRawSamples",
		"tests/performance/harness_test.go:TestHarnessRecordsZeroGateValuesAndTheCeiling",
	}},
	"PER-004": {"component-real", []string{
		"tests/performance/per004_burst_test.go:TestPER004BurstTriggerQueue",
		"tests/performance/per004_burst_test.go:TestPER004BackpressureDisabledFailsTheBoundedBufferGate",
		"tests/performance/harness_test.go:TestHarnessWritesNothingWithoutAProfile",
		"tests/performance/harness_test.go:TestHarnessVacuousGateFails",
	}},
}

// TestE18CatalogMaterialized is the E18 catalog gate: all seven cases this epic owns are materialized,
// resolve to in-tree proofs, declare a class their proofs support, and NAME both their local seam and their
// §6 operator leg (the E14..E17 precedent, plan §T10).
func TestE18CatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedE18Catalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c e18Case
		if err := yaml.Unmarshal(raw, &c); err != nil {
			t.Errorf("%s: decode case.yaml: %v", id, err)
			continue
		}
		checkCase(t, root, id, want.class, want.proofs, c)
	}

	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range e18IDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedE18Catalog[e.Name()]; !ok {
					t.Errorf("%s: an E18-family case dir is not in expectedE18Catalog (add it, or it escapes proof resolution)", e.Name())
				}
				break
			}
		}
	}
}

// checkCase applies every catalog rule to one decoded case. It returns the problems as t.Errorf calls on the
// passed *testing.T, which is what lets TestE18CatalogGuardBites drive it against a deliberately broken case
// on a throwaway T and assert the gate actually refuses.
func checkCase(t *testing.T, root, id, class string, wantProofs []string, c e18Case) {
	t.Helper()
	if c.ID != id {
		t.Errorf("%s: id = %q, want the directory name", id, c.ID)
	}
	if c.ProofClass != class {
		t.Errorf("%s: proof_class = %q, want %q", id, c.ProofClass, class)
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

	// Plan §T10 (the E14..E17 precedent): the case text names its LOCAL seam and its §6 operator leg.
	lower := strings.ToLower(c.Input)
	named := false
	for _, word := range localSeamWords {
		if strings.Contains(lower, word) {
			named = true
			break
		}
	}
	if !named {
		t.Errorf("%s: the case text names no LOCAL seam (want one of %v) — plan §T10 requires it", id, localSeamWords)
	}
	if !strings.Contains(c.Input, "§6") {
		t.Errorf("%s: the case text names no §6 operator leg — every E18 case must say what real-infrastructure leg it does NOT cover", id)
	}
	assertProofs(t, root, id, class, wantProofs, c.Proof)
}

// buildClass maps a proof file's //go:build tag to its master-plan §10.2 proof class. It EXTENDS the E17
// mapping with the two tiers this epic added: `security` (scripts/test/security — real hardened OCI
// sandboxes, real cgroups) and `performance` (the T6 harness — a running binary, real containers, a real
// Postgres). Both are component-real: they need a real backing service, which is exactly what the
// dockerBoundClasses rule below is checking a case did not overclaim.
func buildClass(path, body string) string {
	if strings.HasSuffix(path, ".ts") {
		return "e2e-deterministic"
	}
	for _, line := range strings.Split(body, "\n") {
		if constraint, ok := strings.CutPrefix(strings.TrimSpace(line), "//go:build "); ok {
			switch strings.TrimSpace(constraint) {
			case "fault":
				return "fault-live"
			case "component", "security", "performance":
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

// assertProofs checks the case.yaml `proof:` list equals the catalog's expected list and that each reference
// RESOLVES, then applies the tier rule.
func assertProofs(t *testing.T, root, id, class string, want, got []string) {
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
			continue
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

// TestE18CatalogGuardBites is the RED-first negative for the gate above (the tests/docs
// TestThreatModelGuardBites precedent). A gate that has never refused is not a gate: this drives checkCase
// against four deliberately broken cases on a THROWAWAY *testing.T and asserts each is refused.
func TestE18CatalogGuardBites(t *testing.T) {
	root := repoRoot(t)
	good := e18Case{
		ID: "SEC-102", Name: "a-kebab-case-assertion", ProofClass: "unit", Provider: "fake",
		Input:        "the local seam is a deterministic fixture; §6 operator leg 1 is not claimed",
		ExpectStatus: "completed",
		Proof:        []string{"packages/audit/chain_test.go:TestIntactJournalVerifiesGreen"},
	}
	wantProofs := good.Proof

	for _, tc := range []struct {
		what   string
		mutate func(e18Case) e18Case
	}{
		{"no §6 operator leg", func(c e18Case) e18Case {
			c.Input = "the local seam is real postgres and nothing is deferred"
			return c
		}},
		{"no local seam", func(c e18Case) e18Case {
			c.Input = "everything works; §6 covers the rest"
			return c
		}},
		{"a proof that is not in the tree", func(c e18Case) e18Case {
			c.Proof = []string{"packages/audit/chain_test.go:TestThisDoesNotExist"}
			return c
		}},
		{"a name that is not a kebab-case assertion", func(c e18Case) e18Case {
			c.Name = "Audit Integrity Works"
			return c
		}},
	} {
		spy := &testing.T{}
		broken := tc.mutate(good)
		proofs := wantProofs
		if len(broken.Proof) == 1 && broken.Proof[0] != wantProofs[0] {
			proofs = broken.Proof // the catalog and the case agree; the FILE lookup is what must fail
		}
		checkCase(spy, root, "SEC-102", "unit", proofs, broken)
		if !spy.Failed() {
			t.Errorf("the catalog gate ACCEPTED a case with %s — a gate that has never refused is not a gate", tc.what)
		}
	}

	// ...and the honest case still passes, so the negatives above are not passing for a shared reason.
	spy := &testing.T{}
	checkCase(spy, root, "SEC-102", "unit", wantProofs, good)
	if spy.Failed() {
		t.Error("the catalog gate refused a well-formed case — the negatives above prove nothing")
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
