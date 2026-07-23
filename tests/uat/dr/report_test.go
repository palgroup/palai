package dr

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestComputeRPORTO pins the two measurement primitives: RPO is the marker-loss window
// (last committed - last in backup) and RTO is the recovery wall-clock (disaster -> recovered).
func TestComputeRPORTO(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	if got := ComputeRPO(base.Add(5*time.Second), base); got != 5 {
		t.Fatalf("RPO = %v, want 5", got)
	}
	if got := ComputeRTO(base, base.Add(90*time.Second)); got != 90 {
		t.Fatalf("RTO = %v, want 90", got)
	}
	// A backup taken AFTER the last committed marker (impossible in a real drill) must not
	// silently pass as a negative window — Verify catches it, but the primitive stays honest.
	if got := ComputeRPO(base, base.Add(2*time.Second)); got >= 0 {
		t.Fatalf("RPO for backup-after-write = %v, want negative (caught by Verify)", got)
	}
}

// TestVerifyRecomputesAndCatchesFabrication is the anti-fabrication anchor at the T5 tier (the
// same discipline E15 T6's DrillProof verifier applies): a MEASURED RPO/RTO must be re-derivable
// from its RAW timestamps, so a hand-edited value that the raw data does not support FAILS.
func TestVerifyRecomputesAndCatchesFabrication(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	m := NewMeasure(base.Add(5*time.Second), base, base.Add(10*time.Second), base.Add(100*time.Second))
	ev := DrillEvidence{Drills: []DrillResult{{ID: "DR-001", Passed: true, Measure: &m}}}
	if errs := Verify(ev); len(errs) != 0 {
		t.Fatalf("clean evidence must verify from its raw timestamps: %v", errs)
	}

	// Fabricate a "zero data loss" RPO while the raw timestamps still show a 5s window.
	badM := m
	badM.RPOSeconds = 0
	bad := DrillEvidence{Drills: []DrillResult{{ID: "DR-001", Passed: true, Measure: &badM}}}
	if errs := Verify(bad); len(errs) == 0 {
		t.Fatal("a fabricated RPO (not derivable from raw timestamps) must FAIL recompute")
	}

	// Fabricate a shorter RTO than the raw disaster/recovered instants support.
	badR := m
	badR.RTOSeconds = 12
	bad2 := DrillEvidence{Drills: []DrillResult{{ID: "DR-001", Passed: true, Measure: &badR}}}
	if errs := Verify(bad2); len(errs) == 0 {
		t.Fatal("a fabricated RTO must FAIL recompute")
	}

	// A garbled raw timestamp is itself a finding (a verifier cannot recompute from junk).
	badTS := m
	badTS.DisasterAt = "not-a-timestamp"
	bad3 := DrillEvidence{Drills: []DrillResult{{ID: "DR-001", Passed: true, Measure: &badTS}}}
	if errs := Verify(bad3); len(errs) == 0 {
		t.Fatal("an unparseable raw timestamp must be a finding")
	}
}

// TestRenderReportCarriesMeasuredValuesAndCeiling proves the machine-generated report carries the
// measured numbers (not prose), the published self-host targets, the findings/remediation, and the
// honest same-host ceiling — so a reader never mistakes the local drill for a real second-site DR.
func TestRenderReportCarriesMeasuredValues(t *testing.T) {
	base := time.Date(2026, 7, 23, 12, 0, 0, 0, time.UTC)
	m := NewMeasure(base.Add(5200*time.Millisecond), base, base.Add(3*time.Second), base.Add(78*time.Second))
	ev := DrillEvidence{
		GitSHA: "abc1234",
		Seam:   "local same-host two-stack (Docker Desktop)",
		Drills: []DrillResult{
			{ID: "DR-001", Name: "primary loss", Passed: true, Measure: &m, Detail: "pg container + volume destroyed, restored from last backup"},
			{ID: "DR-004", Name: "object corruption", Passed: true, Detail: "per-file sha256 detected the flipped object exactly"},
		},
		Targets:  DefaultTargets(),
		Findings: DefaultFindings(m.RPOSeconds),
	}
	rep := RenderReport(ev)
	for _, want := range []string{
		"DR-001", "DR-004", "5.2", "75", // measured RPO 5.2s, RTO 75s present as numbers
		"Published", "Findings", "Remediation",
		"same", "SaaS", // the honest ceiling names the same-host limit + DR-003 SaaS scope
	} {
		if !strings.Contains(rep, want) {
			t.Fatalf("report is missing %q:\n%s", want, rep)
		}
	}
	// The report must not silently drop a FAILED drill.
	ev.Drills[1].Passed = false
	if !strings.Contains(RenderReport(ev), "FAIL") {
		t.Fatal("a failed drill must be rendered as FAIL")
	}
}

// TestCommittedReportMatchesEvidence catches a hand-edit to EITHER committed artifact on plain
// `go test ./...` — BEFORE E15 T6's DrillProof lands: the committed evidence must recompute clean
// (dr.Verify == 0), and the committed dr-report.md must be EXACTLY RenderReport(committed evidence).
// Regenerate both with PALAI_DR_WRITE_REPORT=1 (a live drill run) — never hand-edit.
func TestCommittedReportMatchesEvidence(t *testing.T) {
	evPath := filepath.Join("..", "..", "..", "evidence", "dr", "drill-evidence.json")
	repPath := filepath.Join("..", "..", "..", "docs", "operations", "dr-report.md")
	raw, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatalf("read committed evidence: %v", err)
	}
	var ev DrillEvidence
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatalf("decode committed evidence: %v", err)
	}
	if errs := Verify(ev); len(errs) != 0 {
		t.Fatalf("committed evidence does not recompute (a fabricated/edited RPO or RTO): %v", errs)
	}
	report, err := os.ReadFile(repPath)
	if err != nil {
		t.Fatalf("read committed report: %v", err)
	}
	if got := RenderReport(ev); got != string(report) {
		t.Fatalf("committed dr-report.md is NOT RenderReport(committed evidence) — a hand-edit to one of them was made; regenerate both with PALAI_DR_WRITE_REPORT=1")
	}
}
