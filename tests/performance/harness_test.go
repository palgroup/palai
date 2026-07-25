//go:build performance

// The harness's own gate mechanics, including the RED negative the whole tier rests on: a run with no
// hardware/load profile produces NO result. These tests need neither the stack nor a load, so they run
// as `make test-performance TEST=negatives` and are the cheapest proof that the gate has teeth.
package performance

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHarnessRefusesAnUnstampableMachine drives the REAL detection path with no executables on
// PATH: uname/sysctl/docker cannot answer, so the run never opens. This is the profile rule at its
// source — not a flag a caller may forget to set.
func TestHarnessRefusesAnUnstampableMachine(t *testing.T) {
	t.Setenv("PATH", "")
	run, err := NewRun("PER-000", "no load", "test fixture")
	if !errors.Is(err, ErrNoProfile) {
		t.Fatalf("NewRun error = %v, want ErrNoProfile", err)
	}
	if run != nil {
		t.Fatal("NewRun returned a usable run despite an unstampable machine")
	}
}

// TestHarnessWritesNothingWithoutAProfile is the negative in its strong form: a run that HAS samples
// but whose profile lost one required field must produce zero artifacts. A harness that wrote the
// samples anyway would leak an unattributable number into evidence.
func TestHarnessWritesNothingWithoutAProfile(t *testing.T) {
	run := stampedRun(t, "PER-000-noprofile")
	run.Latency("fixture_op", "warm", 5*time.Millisecond, true)

	for _, blank := range []struct {
		field string
		clear func(*Profile)
	}{
		{"docker", func(p *Profile) { p.Docker = "" }},
		{"machine", func(p *Profile) { p.Machine = "" }},
		{"cores", func(p *Profile) { p.Cores = 0 }},
		{"memory_bytes", func(p *Profile) { p.MemoryBytes = 0 }},
		{"load_shape", func(p *Profile) { p.LoadShape = "" }},
		{"ceiling", func(p *Profile) { p.Ceiling = "" }},
	} {
		t.Run(blank.field, func(t *testing.T) {
			saved := run.profile
			blank.clear(&run.profile)
			defer func() { run.profile = saved }()

			dir := filepath.Join(t.TempDir(), "out")
			if _, err := run.Complete(dir); !errors.Is(err, ErrNoProfile) {
				t.Fatalf("Complete error = %v, want ErrNoProfile", err)
			}
			if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
				t.Fatalf("Complete wrote %d artifact(s) for a profile missing %s; want none", len(entries), blank.field)
			}
		})
	}

	// The control arm: with the profile intact the SAME samples do produce a result, so the refusal
	// above is the missing profile and not a broken harness.
	dir := filepath.Join(t.TempDir(), "out")
	if _, err := run.Complete(dir); err != nil {
		t.Fatalf("Complete with a full profile error = %v, want success", err)
	}
	for _, name := range []string{"profile.json", "samples.jsonl", "summary.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

// TestHarnessPercentilesRecomputeFromTheRawSamples is the recompute-over-copy property E18 T10's
// verifier depends on: re-parsing samples.jsonl from disk and re-deriving must reproduce summary.json's
// percentiles exactly, and the summary's samples digest must match those bytes.
func TestHarnessPercentilesRecomputeFromTheRawSamples(t *testing.T) {
	run := stampedRun(t, "PER-000-recompute")
	for i := 1; i <= 100; i++ {
		run.Latency("fixture_op", "warm", time.Duration(i)*time.Millisecond, true)
	}
	run.Gate("fixture_op", 95, float64(200*time.Millisecond), "ns", "test fixture budget")

	dir := filepath.Join(t.TempDir(), "out")
	written, err := run.Complete(dir)
	if err != nil {
		t.Fatalf("Complete error = %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(dir, "samples.jsonl"))
	if err != nil {
		t.Fatalf("read samples: %v", err)
	}
	if got := digest(raw); got != written.SamplesSHA256 {
		t.Fatalf("samples digest = %s, summary claims %s", got, written.SamplesSHA256)
	}
	samples, err := ParseSamples(raw)
	if err != nil {
		t.Fatalf("ParseSamples error = %v", err)
	}
	recomputed := Summarize(samples, map[string]Threshold{"fixture_op": {Percentile: 95, Max: float64(200 * time.Millisecond), Unit: "ns"}}, 0)
	if len(recomputed.Stats) != 1 {
		t.Fatalf("recomputed %d stats, want 1", len(recomputed.Stats))
	}
	got, want := recomputed.Stats[0], written.Stats[0]
	if got.P50 != want.P50 || got.P95 != want.P95 || got.P99 != want.P99 {
		t.Fatalf("recomputed p50/p95/p99 = %v/%v/%v, summary says %v/%v/%v",
			got.P50, got.P95, got.P99, want.P50, want.P95, want.P99)
	}
	// Nearest-rank over 1..100ms: p50 = 50ms, p95 = 95ms, p99 = 99ms. Pinned so a change of method is
	// a test failure, not a silent shift in every published number.
	for _, c := range []struct {
		name string
		got  float64
		want time.Duration
	}{{"p50", got.P50, 50 * time.Millisecond}, {"p95", got.P95, 95 * time.Millisecond}, {"p99", got.P99, 99 * time.Millisecond}} {
		if math.Abs(c.got-float64(c.want.Nanoseconds())) > 1 {
			t.Fatalf("%s = %v, want %v (nearest-rank)", c.name, time.Duration(int64(c.got)), c.want)
		}
	}

	// A doctored summary is caught by the same recomputation: T10 reads the raw file, not this field.
	var onDisk Summary
	body, err := os.ReadFile(filepath.Join(dir, "summary.json"))
	if err != nil {
		t.Fatalf("read summary: %v", err)
	}
	if err := json.Unmarshal(body, &onDisk); err != nil {
		t.Fatalf("decode summary: %v", err)
	}
	onDisk.Stats[0].P95 = 1 // a fabricated, flattering percentile
	if onDisk.Stats[0].P95 == recomputed.Stats[0].P95 {
		t.Fatal("a fabricated p95 survived recomputation; the summary copy is being trusted")
	}
	if !onDisk.DerivedFromRaw || onDisk.SamplesFile != "samples.jsonl" {
		t.Fatalf("summary does not advertise its raw source: %+v", onDisk)
	}
	if onDisk.ReferenceHardware || !onDisk.NoSLOClaim {
		t.Fatal("summary must carry the no-SLO / no-reference-hardware stamp")
	}
}

// TestHarnessGateFailsAndStillWritesTheEvidence: a breached threshold fails the run but keeps the artifacts —
// a failed measurement is evidence, and the RED fixtures read it back.
func TestHarnessGateFailsAndStillWritesTheEvidence(t *testing.T) {
	run := stampedRun(t, "PER-000-gate")
	for i := 0; i < 10; i++ {
		run.Latency("fixture_op", "warm", 900*time.Millisecond, true)
	}
	run.Gate("fixture_op", 95, float64(100*time.Millisecond), "ns", "test fixture budget")

	dir := filepath.Join(t.TempDir(), "out")
	sum, err := run.Complete(dir)
	if !errors.Is(err, ErrThreshold) {
		t.Fatalf("Complete error = %v, want ErrThreshold", err)
	}
	if sum.Pass || len(sum.Failures) == 0 {
		t.Fatalf("summary = %+v, want a failing gate with a stated reason", sum)
	}
	if _, err := os.Stat(filepath.Join(dir, "samples.jsonl")); err != nil {
		t.Fatalf("a failing run must still leave its raw samples: %v", err)
	}
}

// TestHarnessVacuousGateFails closes the hole where a harness "passes" because it measured nothing: a gate on a
// metric with zero samples is a failure, not a pass.
func TestHarnessVacuousGateFails(t *testing.T) {
	run := stampedRun(t, "PER-000-vacuous")
	run.Latency("measured", "warm", time.Millisecond, true)
	run.Gate("never_measured", 95, 1, "ns", "test fixture budget")

	if _, err := run.Complete(filepath.Join(t.TempDir(), "out")); !errors.Is(err, ErrThreshold) {
		t.Fatalf("Complete error = %v, want ErrThreshold for a gate with no samples", err)
	}
}

// TestHarnessThresholdOverrideIsRecorded proves the thresholds are configurable AND that a loosened run says so
// in its own evidence — an override that hid itself would let a number pass by editing the gate.
func TestHarnessThresholdOverrideIsRecorded(t *testing.T) {
	t.Setenv("PALAI_PERF_MAX_FIXTURE_OP", "2s")
	run := stampedRun(t, "PER-000-override")
	run.Latency("fixture_op", "warm", time.Second, true)
	run.Gate("fixture_op", 95, float64(100*time.Millisecond), "ns", "test fixture budget")

	sum, err := run.Complete(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatalf("Complete error = %v, want the override to admit a 1s sample", err)
	}
	if sum.Stats[0].Gate == nil || !strings.HasPrefix(sum.Stats[0].Gate.Source, "env PALAI_PERF_MAX_FIXTURE_OP=") {
		t.Fatalf("gate source = %+v, want the environment override recorded", sum.Stats[0].Gate)
	}
}

// stampedRun opens a real profiled run (the machine IS stamped here — that is the point of the control
// arms above) and fails the test if the machine cannot be described.
func stampedRun(t *testing.T, caseID string) *Run {
	t.Helper()
	run, err := NewRun(caseID, "harness self-check: no load", "harness self-check; produces no published number")
	if err != nil {
		t.Fatalf("NewRun error = %v (the performance tier needs a stampable machine + a Docker daemon)", err)
	}
	return run
}
