// Package evals is the E17 T6 eval harness: a content-addressed fixture format + a deterministic Go runner
// that scores four suites (coding, research/citation, recovery, security/red-team) and feeds a release
// gate. It is release MACHINERY, not a discovery capability — nothing here advertises a capability tier.
//
// HONEST CEILING (plan §T6, E08 rule — stated loudly because it is load-bearing): the reference engine
// this harness runs opens NO tool to a real provider, so NO real-model agentic benchmark runs here. The
// scores prove the HARNESS + the graders + the threshold-gate MECHANICS, NOT model quality. "Thresholds
// met" is a gate-mechanics claim, never a model-quality claim. Real-model quality numbers are §6 leg 7 and
// an E18 RC input. A live subset (single-step research/citation against a real provider + a model-judge
// calibration smoke) lives behind the `live` build tag and proves only that the harness can score a real
// single call — still not an agentic benchmark.
package evals

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Split is the file-level train/validation/held-out separation (plan §T6). It is the fixture's PARENT
// DIRECTORY name, never a field inside the fixture — a case cannot lie about which split it belongs to.
// ONLY the held-out split feeds the release gate; train/validation are for authoring/tuning and never gate.
type Split string

const (
	Train      Split = "train"
	Validation Split = "validation"
	HeldOut    Split = "held-out"
)

// GraderKind is the grader-priority vocabulary (§57.6). Deterministic kinds (invariant/schema/test/cost)
// are AUTHORITATIVE and outrank trace, which outranks model-judge. A model-judge grade is CALIBRATION ONLY
// and can NEVER be the sole gate for a protected class (ValidateDataset enforces this).
type GraderKind string

const (
	GradeInvariant  GraderKind = "invariant"   // deterministic: named signals equal the reference values
	GradeSchema     GraderKind = "schema"      // deterministic: output shape / signals present
	GradeTest       GraderKind = "test"        // deterministic: a hidden-test signal holds
	GradeCost       GraderKind = "cost"        // deterministic: signals hold AND cost <= budget
	GradeTrace      GraderKind = "trace"       // ordered-trace grader (below deterministic, above judge)
	GradeModelJudge GraderKind = "model-judge" // calibrated score; never a sole protected-class gate
)

// deterministic reports whether a kind is an authoritative deterministic grader (§57.6 priority head).
func (k GraderKind) deterministic() bool {
	switch k {
	case GradeInvariant, GradeSchema, GradeTest, GradeCost:
		return true
	default:
		return false
	}
}

// ProtectedClass marks a case whose verdict a model-judge may NEVER decide alone (§57.6): destructive
// safety, secret handling, tenant isolation, protocol conformance. Empty = unprotected (a model-judge may
// grade it for calibration). ValidateDataset rejects a protected case graded solely by model-judge.
type ProtectedClass string

const (
	NotProtected     ProtectedClass = ""
	ClassDestructive ProtectedClass = "destructive-safety"
	ClassSecret      ProtectedClass = "secret"
	ClassTenant      ProtectedClass = "tenant"
	ClassProtocol    ProtectedClass = "protocol"
)

// securitySignals are the outcome signals whose WRONG value is a security regression the gate blocks on,
// independent of the aggregate score (§57.13). A case that expects any of these is a security case for the
// SecurityFailures tally, regardless of which suite it lives in (a coding secret-leak case counts too).
var securitySignals = map[string]bool{
	"followed_injection":   true,
	"secret_leaked":        true,
	"ssrf_blocked":         true,
	"cross_tenant_blocked": true,
	"approval_required":    true,
}

// EvalCase is one immutable eval fixture. It is loaded from a JSON file; its Split is the parent directory,
// not a field. Expect holds the reference signal values the deterministic grader compares the candidate
// against — the SAFE-correct behaviour. Injected is an instruction embedded in the (untrusted) input a
// safe engine must IGNORE; HasSecret marks a fixture that dangles a secret a safe engine must never emit.
type EvalCase struct {
	ID        string          `json:"id"`
	Suite     string          `json:"suite"`
	Grader    GraderKind      `json:"grader"`
	Protected ProtectedClass  `json:"protected"`
	Input     string          `json:"input"`
	Injected  string          `json:"injected,omitempty"`
	HasSecret bool            `json:"has_secret,omitempty"`
	Expect    map[string]bool `json:"expect"`
	Budget    int             `json:"budget,omitempty"`

	split Split // set from the directory at load time; not serialized
}

// SplitOf returns the split the case was loaded from.
func (c EvalCase) SplitOf() Split { return c.split }

// isSecurityCase reports whether this case's verdict counts toward the SecurityFailures tally: it lives in
// the security suite OR expects any security signal (so a coding secret-like fixture is counted too).
func (c EvalCase) isSecurityCase() bool {
	if c.Suite == "security" {
		return true
	}
	for k := range c.Expect {
		if securitySignals[k] {
			return true
		}
	}
	return false
}

// DatasetRevision is an immutable, content-addressed set of cases for one suite+split. Digest is the
// content address: sha256 over the canonically-sorted, canonically-serialized cases, so an edit to any
// fixture changes the digest (the immutability the gate's per-suite score digest relies on).
type DatasetRevision struct {
	Suite string
	Split Split
	Cases []EvalCase
}

// Digest is the content address of the revision (sha256:<hex>). It sorts cases by ID and serializes the
// grading-relevant fields canonically, so it is stable across load order and changes iff a fixture changes.
func (d DatasetRevision) Digest() string {
	type canon struct {
		ID        string          `json:"id"`
		Suite     string          `json:"suite"`
		Grader    GraderKind      `json:"grader"`
		Protected ProtectedClass  `json:"protected"`
		Input     string          `json:"input"`
		Injected  string          `json:"injected"`
		HasSecret bool            `json:"has_secret"`
		Expect    map[string]bool `json:"expect"`
		Budget    int             `json:"budget"`
	}
	cs := make([]canon, len(d.Cases))
	for i, c := range d.Cases {
		cs[i] = canon{c.ID, c.Suite, c.Grader, c.Protected, c.Input, c.Injected, c.HasSecret, c.Expect, c.Budget}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].ID < cs[j].ID })
	raw, _ := json.Marshal(struct {
		Suite string  `json:"suite"`
		Split Split   `json:"split"`
		Cases []canon `json:"cases"`
	}{d.Suite, d.Split, cs})
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ValidateDataset enforces the §57.6 grader-priority rule at load time: a protected case (destructive /
// secret / tenant / protocol) may NOT be graded SOLELY by a model-judge — a calibrated judge can never be
// the gate for those classes. A malformed fixture (missing id/suite/grader/expect) is also rejected.
func ValidateDataset(d DatasetRevision) error {
	for _, c := range d.Cases {
		if c.ID == "" || c.Suite == "" || c.Grader == "" || len(c.Expect) == 0 {
			return fmt.Errorf("eval fixture is malformed: id/suite/grader/expect must all be set (got id=%q suite=%q grader=%q expect=%d)", c.ID, c.Suite, c.Grader, len(c.Expect))
		}
		if c.Protected != NotProtected && c.Grader == GradeModelJudge {
			return fmt.Errorf("eval fixture %q is a protected class (%s) graded SOLELY by model-judge — §57.6 forbids a model-judge as the sole gate for destructive-safety/secret/tenant/protocol", c.ID, c.Protected)
		}
	}
	return nil
}

// LoadSuite loads one suite+split from testdata/<suite>/<split>/*.json into a DatasetRevision and validates
// it. The split is taken from the directory, never from the file. Every suite this repo ships is validated,
// so a §57.6 violation authored into a fixture fails the harness rather than silently under-gating.
func LoadSuite(root, suite string, split Split) (DatasetRevision, error) {
	dir := filepath.Join(root, suite, string(split))
	entries, err := os.ReadDir(dir)
	if err != nil {
		return DatasetRevision{}, fmt.Errorf("read %s/%s: %w", suite, split, err)
	}
	d := DatasetRevision{Suite: suite, Split: split}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return DatasetRevision{}, err
		}
		var c EvalCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return DatasetRevision{}, fmt.Errorf("%s/%s/%s: %w", suite, split, e.Name(), err)
		}
		if c.Suite != suite {
			return DatasetRevision{}, fmt.Errorf("%s/%s/%s: suite=%q does not match its directory %q", suite, split, e.Name(), c.Suite, suite)
		}
		c.split = split
		d.Cases = append(d.Cases, c)
	}
	if err := ValidateDataset(d); err != nil {
		return DatasetRevision{}, err
	}
	return d, nil
}

// Suites is the fixed set of the four eval suites (plan §T6). It is the enumeration the runner and the
// harness proofs iterate; recovery TARGETS the existing fault harness (see recovery fixtures) rather than
// re-running Docker kills here.
var Suites = []string{"coding", "research", "recovery", "security"}
