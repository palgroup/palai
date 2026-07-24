// Package automation holds the E11 automation UAT catalog gate + the automation-0.1.0 evidence-release
// verification. Both are Docker-free pure checks, so they ride `make verify` (no credential, no stack):
// the catalog gate asserts every automation case this slice owns is MATERIALIZED — present, honestly
// named, declaring a valid proof class, and pointing at an in-tree proof whose //go:build tier equals the
// declared class — and the evidence gate asserts the committed automation-0.1.0 bundle verifies clean
// through the shared verifier (0 findings, 0 secret findings) with all four automation rules active.
//
// ponytail: this file is a small copy-adaptation of tests/uat/recovery/catalog_test.go (recCase/
// validProofClasses/honestNamePattern/buildClass/assertProofsMatch/repoRoot). Exporting those helpers to
// a shared tests/uat package is a separate refactor, not this task.
package automation

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// autoCase is the case.yaml catalog record for an automation case — identical shape to the recovery
// recCase: a structured `proof:` list so the gate can assert the referenced proof genuinely exists in the
// tree at the declared tier. A case may not claim a half that is not already proven by T1-T6.
type autoCase struct {
	ID           string   `yaml:"id"`
	Name         string   `yaml:"name"`
	ProofClass   string   `yaml:"proof_class"`
	Provider     string   `yaml:"provider"`
	Input        string   `yaml:"input"`
	ExpectStatus string   `yaml:"expect_status"`
	Proof        []string `yaml:"proof"`
}

// validProofClasses is the master-plan §10.2 vocabulary an automation case may declare.
var validProofClasses = map[string]bool{
	"unit": true, "component-real": true, "e2e-deterministic": true,
	"live-provider": true, "external-receipt": true, "fault-live": true,
}

var honestNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// automationIDPrefixes are the case-id families E11 EXCLUSIVELY owns; every case.yaml under one of them
// must be in expectedAutomationCatalog, so an automation case cannot be added outside the map and escape
// proof resolution. AGT-003 is deliberately NOT opened (E08 core-loop ownership); it never gets a dir.
var automationIDPrefixes = []string{"AUT-", "AGT-"}

// expectedAutomationCatalog is the E11 automation UAT catalog: every case ID this slice materializes
// (spec §20-21, §33 acceptance contract AGT/AUT), mapped to the proof class its case.yaml must declare and
// the in-tree proof(s) that already prove it (T1-T6). The declared class is the BUILD-TAG TIER of the
// referenced proof — MECHANICALLY enforced by assertProofsMatch reading each proof file's //go:build tag —
// so a reader can run exactly that tier to see the proof and no case can overclaim its tier. A missing dir,
// a drifted class, a tag/class mismatch, or a proof reference that does not resolve fails the gate: the
// automation catalog cannot silently under-materialize or overclaim a proven half.
//
// HONEST NAMING (spec §10.2, the gate writes the mechanical tier): AUT-012's in-tree proof is the TAG-LESS
// adapter test (real local HTTP + injected resolver, runs in make verify) → declared unit, not the plan
// prose's "component-real" — overclaiming the tier would break the gate. AUT-006 is unit (real tzdata, not
// a component DB). AUT-003's in-tree proof is component-tagged (real PG + real admission); its composed-
// binary half lives in the wiring component test + the journey — the case.yaml `input:` names that ceiling.
var expectedAutomationCatalog = map[string]struct {
	class  string
	proofs []string
}{
	// AGT — agent profile/revision publish immutability + exact-revision pin (spec §19, E11 Task 1).
	// AGT-003 (profile-scoped run authorization) is E08 core-loop territory and is NOT opened here.
	"AGT-001": {"component-real", []string{
		"apps/control-plane/internal/automation/agents_component_test.go:TestAgentRevisionPublishIsImmutable",
		"tests/component/postgres/agent_pin_admission_test.go:TestUnpublishedRevisionCannotBePinnedOrRun",
	}},
	"AGT-002": {"component-real", []string{"apps/control-plane/internal/automation/triggers_component_test.go:TestAcceptedDeliveryPinsExactRevision"}},

	// AUT — trigger/inbound/schedule/callback automation invariants (spec §20-21, §33, E11 Task 2-6).
	"AUT-001": {"component-real", []string{
		"apps/control-plane/internal/automation/dedupe_component_test.go:TestDuplicateDeliveryLinksOriginalSingleAction",
		"apps/control-plane/internal/automation/inbound_component_test.go:TestDuplicateSourceEventSingleActionOriginalLinkage",
	}},
	"AUT-002": {"component-real", []string{"apps/control-plane/internal/automation/inbound_component_test.go:TestInvalidSignatureRejectedBeforePersistence"}},
	"AUT-003": {"component-real", []string{"apps/control-plane/internal/automation/map_admit_component_test.go:TestMappingFailureFailedDeliveryNoRunEndToEnd"}},
	"AUT-004": {"component-real", []string{"apps/control-plane/internal/automation/queue_component_test.go:TestQueuePolicyOrdersRunsPerKey"}},
	"AUT-005": {"component-real", []string{
		"apps/control-plane/internal/automation/concurrency_component_test.go:TestConcurrencyPoliciesDocumentedOutcomes",
		"apps/control-plane/internal/automation/guard_component_test.go:TestReplaceDeniedAfterIrreversibleToolCall",
		"apps/control-plane/internal/automation/guard_component_test.go:TestCoalesceDeniedAfterIrreversibleToolCall",
	}},
	"AUT-006": {"unit", []string{
		"apps/control-plane/internal/automation/cron_next_test.go:TestNextOccurrenceDSTDuplicateFiresOnceEarlierInstant",
		"apps/control-plane/internal/automation/cron_next_test.go:TestNextOccurrenceDSTGapSkipsToNextValid",
	}},
	"AUT-007": {"component-real", []string{"apps/control-plane/internal/automation/scheduler_component_test.go:TestTwoSchedulerReplicasSingleCanonicalOccurrence"}},
	"AUT-008": {"fault-live", []string{
		"apps/control-plane/internal/automation/fault/outage_test.go:TestSchedulerOutageFireOnceNowLatestMissed",
		"apps/control-plane/internal/automation/fault/outage_test.go:TestSchedulerOutageCatchUpBoundedOldestFirst",
	}},
	"AUT-009": {"component-real", []string{
		"apps/control-plane/internal/automation/inbound_component_test.go:TestRedeliveryAfterLostAckDoesNotDuplicate",
		// Queue-adapter leg (E17 T7): redelivery-after-lost-ack via the durable queue adapter.
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterRedeliversAfterLostAckSingleEffect",
	}},
	"AUT-010": {"component-real", []string{
		"apps/control-plane/internal/automation/inbound_component_test.go:TestFloodBoundsMemoryReportsDepthApplies429",
		// Queue-adapter leg (E17 T7): flood -> bounded-buffer backpressure via the queue adapter.
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterFloodAppliesBackpressureNoDrop",
	}},
	"AUT-011": {"component-real", []string{
		"apps/control-plane/internal/automation/webhook_component_test.go:TestSignedDeliveryEndToEndRealHTTP",
		"apps/control-plane/internal/automation/callback_component_test.go:TestCallbackFailureLeavesRunTerminalIntact",
	}},
	"AUT-012": {"unit", []string{
		"adapters/integrations/webhook/sender_test.go:TestPrivateAndLoopbackDestinationsDeniedByDefault",
		"adapters/integrations/webhook/sender_test.go:TestDNSRebindingReResolveDeniesFlippedTarget",
		"adapters/integrations/webhook/sender_test.go:TestRedirectNotFollowed",
	}},
	"AUT-013": {"component-real", []string{
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetrySameIdempotencyKeySingleEverything",
		"apps/control-plane/internal/automation/idempotency_component_test.go:TestOrchestratorRetryDifferentKeySameDedupeSingleAction",
		// Queue-adapter leg (E17 T7): same idempotency key -> single effect via the append-only receipts
		// ledger. The orchestrator-kit leg (§35) is T8's, appended separately.
		"apps/control-plane/internal/automation/queue_adapter_component_test.go:TestQueueAdapterRedeliversAfterLostAckSingleEffect",
	}},
}

// TestAutomationCatalogMaterialized is the E11 automation-catalog gate: every proven half from T1-T6 has a
// case.yaml that names it honestly, declares the proof class of the tier that runs it, and points at an
// in-tree proof that actually exists. It rides make verify (no Docker), so a forgotten case, an overclaimed
// class, or a case that references a proof not in the tree fails fast.
func TestAutomationCatalogMaterialized(t *testing.T) {
	root := repoRoot(t)
	casesDir := filepath.Join(root, "tests", "uat", "cases")

	for id, want := range expectedAutomationCatalog {
		raw, err := os.ReadFile(filepath.Join(casesDir, id, "case.yaml"))
		if err != nil {
			t.Errorf("%s: read case.yaml: %v", id, err)
			continue
		}
		var c autoCase
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

	// Orphan guard: every automation-family case dir must be in the map (no case escapes proof resolution).
	entries, err := os.ReadDir(casesDir)
	if err != nil {
		t.Fatalf("read cases dir: %v", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		for _, prefix := range automationIDPrefixes {
			if strings.HasPrefix(e.Name(), prefix) {
				if _, ok := expectedAutomationCatalog[e.Name()]; !ok {
					t.Errorf("%s: an automation-family case dir is not in expectedAutomationCatalog (add it, or it escapes proof resolution)", e.Name())
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
