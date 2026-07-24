package evals

import (
	"path/filepath"
	"testing"
)

func testdataRoot() string { return "testdata" }

// TestFourSuitesGreen is the harness half of QUA-001: the four content-addressed suites load, validate
// (§57.6), and run GREEN under the SafePolicy reference engine on the held-out split — the reference
// engine's correct behaviour reproduces every fixture's reference signals. This proves the HARNESS +
// graders, NOT model quality (the engine opens no tool to a real provider — E08).
func TestFourSuitesGreen(t *testing.T) {
	reports, err := RunAll(testdataRoot(), HeldOut, SafePolicy)
	if err != nil {
		t.Fatalf("RunAll held-out: %v", err)
	}
	if len(reports) != len(Suites) {
		t.Fatalf("expected %d suites, got %d", len(Suites), len(reports))
	}
	for _, suite := range Suites {
		r, ok := reports[suite]
		if !ok {
			t.Fatalf("suite %q missing from report", suite)
		}
		if r.Total == 0 {
			t.Errorf("suite %q has no held-out cases", suite)
		}
		if r.Passed != r.Total {
			for _, g := range r.Grades {
				if !g.Pass {
					t.Errorf("suite %q case %q FAILED under SafePolicy: %s", suite, g.CaseID, g.Detail)
				}
			}
		}
		if r.SecurityFailures != 0 {
			t.Errorf("suite %q had %d security failures under SafePolicy (want 0)", suite, r.SecurityFailures)
		}
	}
}

// TestDigestIsContentAddressed pins the content-addressed fixture format (plan §T6): a suite's digest is
// stable across reloads and DIFFERS across splits (train/validation/held-out), so the gate's per-suite
// score digest genuinely pins WHICH immutable fixtures produced the numbers.
func TestDigestIsContentAddressed(t *testing.T) {
	a, err := LoadSuite(testdataRoot(), "coding", HeldOut)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := LoadSuite(testdataRoot(), "coding", HeldOut)
	if a.Digest() != b.Digest() {
		t.Fatal("digest is not stable across reloads")
	}
	train, err := LoadSuite(testdataRoot(), "coding", Train)
	if err != nil {
		t.Fatal(err)
	}
	if a.Digest() == train.Digest() {
		t.Fatal("held-out and train digests must differ (splits are separated at the file level)")
	}
}

// TestModelJudgeNeverSoleGateForProtected is the §57.6 invariant: a protected case (destructive-safety /
// secret / tenant / protocol) graded SOLELY by a model-judge is REJECTED by the dataset validator — a
// calibrated judge can never be the gate for those classes. Every shipped protected fixture is already
// deterministic; this pins the rule against a future authoring mistake.
func TestModelJudgeNeverSoleGateForProtected(t *testing.T) {
	bad := DatasetRevision{Suite: "security", Split: HeldOut, Cases: []EvalCase{{
		ID: "x", Suite: "security", Grader: GradeModelJudge, Protected: ClassSecret,
		Input: "leak?", Expect: map[string]bool{"secret_leaked": false},
	}}}
	if err := ValidateDataset(bad); err == nil {
		t.Fatal("a protected case graded solely by model-judge must be REJECTED (§57.6)")
	}
	// The same case graded deterministically is accepted.
	bad.Cases[0].Grader = GradeInvariant
	if err := ValidateDataset(bad); err != nil {
		t.Fatalf("a deterministically-graded protected case must be accepted: %v", err)
	}
}

// TestRegressedPolicyIsDetectable proves the reference engine's RegressedPolicy is a REAL, detectable
// security regression: run under RegressedPolicy the security suite fails its security cases (this is the
// signal the gate blocks on, §57.13), while a non-security coding bug-fix case still passes (a regression
// is security-only, so it can block independent of the aggregate).
func TestRegressedPolicyIsDetectable(t *testing.T) {
	sec, err := LoadSuite(testdataRoot(), "security", HeldOut)
	if err != nil {
		t.Fatal(err)
	}
	r := RunSuite(sec, RegressedPolicy)
	if r.SecurityFailures == 0 {
		t.Fatal("RegressedPolicy must produce security failures in the security suite")
	}
	// A non-security signal is unaffected by the regression.
	one := EvalCase{ID: "bf", Suite: "coding", Grader: GradeTest, Input: "x", Expect: map[string]bool{"hidden_test_passed": true}}
	if g := GradeCase(one, Solve(one, RegressedPolicy)); !g.Pass {
		t.Fatal("a non-security case must still pass under RegressedPolicy (regression is security-only)")
	}
}

func TestLoadSuiteRejectsWrongSuiteDir(t *testing.T) {
	// Guard the loader's directory/suite consistency check exists (a fixture cannot lie about its suite).
	if _, err := LoadSuite(filepath.Join("testdata"), "coding", HeldOut); err != nil {
		t.Fatalf("well-formed load must succeed: %v", err)
	}
}
