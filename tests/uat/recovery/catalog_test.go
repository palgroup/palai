// Package recovery holds the E10 recovery UAT catalog gate + the recovery-0.1.0 evidence-release
// verification. Both are Docker-free pure checks, so they ride `make verify` (no credential, no stack):
// the catalog gate asserts every recovery case this slice owns is MATERIALIZED — present, honestly
// named, declaring a valid proof class, and pointing at an in-tree proof that actually exists — and the
// evidence gate asserts the committed recovery-0.1.0 bundle verifies clean through the shared verifier
// (0 findings, 0 secret findings) with its §26.12 RecoveryProof rule active.
package recovery

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// recCase is the case.yaml catalog record for a recovery case. It extends the coding catalogCase with a
// structured `proof:` list so the gate can assert the referenced proof genuinely exists in the tree — a
// case may not claim a recovery half that is not already proven by T1-T8.
type recCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary a recovery case may declare, identical to the
// coding catalog gate's set.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// recoveryIDPrefixes are the case-id families E10 EXCLUSIVELY owns; every case.yaml under one of them must
// be in expectedRecoveryCatalog, so a recovery case cannot be added outside the map and escape proof
// resolution. SAN is deliberately absent: it is shared with E09 (SAN-001..004 are E09 sandbox cases), so a
// prefix sweep can't own it — the SAN-005..008 recovery cases are still validated as map keys above.
// TOL- is ALSO absent: E12 T10 materialized the extensibility half of the TOL- family (TOL-008..012/018),
// so the extensibility catalog (tests/uat/extensibility/catalog_test.go) is now the SINGLE owner of TOL-
// orphan resolution — it allowlists the recovery-only TOL-001..004 and maps the shared TOL-016/017. This
// gate's map loop still validates the recovery-owned TOL cases below; it just no longer sweeps TOL- dirs.
// ponytail: exclusive-family sweep only; a shared-family orphan (e.g. a stray SAN-009) is not auto-caught.
var recoveryIDPrefixes = []string{"ENG-", "SES-", "REC-", "DET-"}

// expectedRecoveryCatalog is the E10 recovery UAT catalog: every case ID this slice materializes (spec §3
// acceptance contract ENG/TOL/SAN/SES + §64 authored REC/DET), mapped to the proof class its case.yaml
// must declare and the in-tree proof(s) that already prove it (T1-T8). The declared class is the BUILD-TAG
// TIER of the referenced proof — MECHANICALLY enforced by assertProofsMatch reading each proof file's
// //go:build tag — so a reader can run exactly that tier to see the proof and no case can overclaim its
// tier. A missing dir, a drifted class, a tag/class mismatch, or a proof reference that does not resolve
// fails the gate: the recovery catalog cannot silently under-materialize or overclaim a proven half.
var expectedRecoveryCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// ENG — engine/host kill + recovery ladder (spec §26.3-26.8).
	"ENG-004": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/kill_matrix_test.go:TestEngineProcessKillRecoversViaLadder"}},
	"ENG-005": {"fault-live", []string{"tests/fault/recovery/container_kill_test.go:TestContainerKillNeverFalseSuccess"}},
	"ENG-006": {"fault-live", []string{"tests/fault/recovery/host_kill_test.go:TestRunnerDaemonKillAdvancesFenceAndRecovers"}},
	"ENG-007": {"component-real", []string{"apps/control-plane/internal/artifacts/workspace_recovery_component_test.go:TestOldHostAuthoritativeFramesDeniedDiagnosticsAllowed"}},
	"ENG-008": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/recovery_ladder_test.go:TestLadderPrefersExactWhenLeaseAlive"}},
	"ENG-009": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/recovery_ladder_test.go:TestCompatibleCheckpointRestoresBoundaryNoToolReplay"}},
	"ENG-010": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/recovery_ladder_test.go:TestIncompatibleCheckpointFallsToTranscriptWithRejectedEvent"}},
	"ENG-011": {"component-real", []string{"apps/control-plane/internal/artifacts/checkpoint_component_test.go:TestCheckpointMigrationPreservesOriginalWithProvenance"}},
	"ENG-012": {"component-real", []string{"apps/control-plane/internal/execution/tool_ledger_component_test.go:TestOutageCommandsDeliverCanonicalOrderAfterRecovery"}},
	"ENG-013": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/kill_matrix_test.go:TestRedeliveredTerminalStaysSingleByMonotonicity"}},
	"ENG-014": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/kill_matrix_test.go:TestExitWithoutTerminalNeverFalseSuccess"}},

	// TOL — tool replay classes + uncertain reconciliation (spec §26.6-26.7). TOL-016/017 are SHARED cases:
	// E12 T10 materialized their signed-transport half, so their proof list now carries BOTH the E10 ledger/
	// fence half AND the E12 signed half — both halves share a tier (016 both unit, 017 both component), so
	// the combined list satisfies this gate AND the extensibility gate off the one case.yaml. The E10 half
	// stays FIRST in the list (E10 claims unchanged); the extensibility gate expects the identical list.
	"TOL-001": {"component-real", []string{"apps/control-plane/internal/execution/tool_ledger_component_test.go:TestPureToolReplayLabeledNoDuplication"}},
	"TOL-002": {"unit", []string{"packages/tool-broker/broker_test.go:TestIdempotentToolSameKeySingleExternalObject"}},
	"TOL-003": {"component-real", []string{"apps/control-plane/internal/execution/tool_ledger_component_test.go:TestIrreversibleUncertainNeverAutoReplays"}},
	"TOL-004": {"unit", []string{"apps/control-plane/internal/execution/reconcile_unit_test.go:TestReversibleReconcilesThenCompensates"}},
	"TOL-016": {"unit", []string{
		"packages/tool-broker/broker_test.go:TestDuplicateToolCallIdSingleExecution",
		"adapters/tools/http/executor_test.go:TestRemoteDuplicateRetrySameToolCallIdSingleExecution",
	}},
	"TOL-017": {"component-real", []string{
		"apps/control-plane/internal/execution/tool_ledger_component_test.go:TestLateCallbackAfterFenceAdvanceDenied",
		"apps/control-plane/internal/execution/remote_prober_component_test.go:TestLateCallbackAfterDeadlineEntersReconciliationNotSilentCommit",
	}},

	// SAN — snapshot restore + host-kill fence + reuse hygiene + quarantine (spec §26.8, §28-29).
	"SAN-005": {"unit", []string{"adapters/sandboxes/oci/snapshot/archive_test.go:TestSnapshotRestoreChecksumsMatchCreate"}},
	"SAN-006": {"fault-live", []string{"tests/fault/recovery/host_kill_test.go:TestHostKillFencesStaleWriter"}},
	"SAN-007": {"component-real", []string{"apps/control-plane/internal/artifacts/workspace_recovery_component_test.go:TestAllocationReuseLeavesNoTenantResidue"}},
	"SAN-008": {"component-real", []string{"apps/control-plane/internal/artifacts/workspace_recovery_component_test.go:TestFailedDestroyQuarantinesHost"}},

	// SES — pause-checkpoint validity + cancel-during-kill recovery (spec §26.10-26.11).
	"SES-009": {"e2e-deterministic", []string{
		"apps/control-plane/e2e/responses/pause_checkpoint_test.go:TestPauseProducesValidCheckpointBeforeComputeRelease",
		"apps/control-plane/e2e/responses/pause_checkpoint_test.go:TestResumeRestoresFromValidCheckpoint",
	}},
	"SES-010": {"component-real", []string{"tests/component/postgres/cancel_recovery_test.go:TestCancelDuringKillReconcilesChildrenSingleTerminal"}},

	// REC — authored recovery invariants (spec §64 catalog reconciliation is a doc carry-over, §7).
	"REC-001": {"fault-live", []string{"tests/fault/recovery/terminal_test.go:TestFastExitEngineTerminalFrameNeverLost"}},
	"REC-002": {"component-real", []string{"apps/control-plane/internal/execution/redelivery_component_test.go:TestAppliedMessageSurvivesCrashBeforeFoldCommit"}},
	"REC-003": {"component-real", []string{"apps/control-plane/internal/execution/redelivery_component_test.go:TestAppliedFoldedTurnPresentInPostResumeHistory"}},
	"REC-004": {"component-real", []string{"apps/control-plane/internal/artifacts/gc_test.go:TestGCDeletesUnreferencedObjectAfterGrace"}},
	"REC-005": {"component-real", []string{"apps/control-plane/internal/artifacts/workspace_recovery_component_test.go:TestHostMoveKeepsLogicalIdNewFencedAllocation"}},
	"REC-006": {"unit", []string{
		"tests/uat/evidence_test.go:TestRecoveryProofFieldsComplete",
		"tests/uat/evidence_test.go:TestVerifierRejectsContinuedLogWithoutProof",
	}},

	// DET — parent-detached durable child + durable parent<->child conversation (master plan §431, §25.18-19).
	"DET-001": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/detached_child_test.go:TestParentReleasesComputeWhileChildRuns"}},
	"DET-002": {"e2e-deterministic", []string{"apps/control-plane/e2e/responses/detached_child_test.go:TestDetachedChildIdleReceivesSpineMessage"}},
}

// TestRecoveryCatalogMaterialized is the E10 recovery-catalog gate: every proven half from T1-T8 has a
// case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at an
// in-tree proof that actually exists. It rides make verify (no Docker), so a forgotten case, an overclaimed
// class, or a case that references a proof not in the tree fails fast.
func TestRecoveryCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedRecoveryCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c recCase
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
		// The materialization guarantee: the case.yaml lists exactly the proof(s) the catalog expects, and
		// each one resolves to a real func in a real file whose build-tag tier equals the declared class.
		assertProofsMatch(t, root, id, want.class, want.proofs, c.Proof)
	}

	// Orphan guard: every recovery-family case dir must be in the map (no case escapes proof resolution).
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range recoveryIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedRecoveryCatalog[e.Name()]; !ok {
					t.Errorf("%s: a recovery-family case dir is not in expectedRecoveryCatalog (add it, or it escapes proof resolution)", e.Name())
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
