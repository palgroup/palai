package evals

import "sort"

// Grade is one case's verdict. Kind records which grader produced it (for the §57.6 priority audit); Pass /
// Score are the deterministic outcome (1.0 / 0.0 for the boolean graders). Security is true when a failure
// here is a security regression the gate must block on independent of the aggregate (§57.13).
type Grade struct {
	CaseID string
	Kind   GraderKind
	// Authoritative is true when the grader is a deterministic head-of-priority kind (§57.6). A
	// non-authoritative (model-judge) grade is calibration only; ValidateDataset already forbids it as a
	// protected case's sole grader, so a false here on a gating verdict never reaches the gate.
	Authoritative bool
	Pass          bool
	Score         float64
	Security      bool
	Detail        string
}

// GradeCase applies the case's grader to a candidate Outcome. Every grader in this harness reduces to a
// DETERMINISTIC signal comparison (the authoritative check, §57.6); the cost grader adds a budget bound;
// the model-judge grader runs the SAME deterministic comparison but is flagged non-authoritative and, by
// ValidateDataset, is never a protected case's sole grader — so a calibrated judge never gates a protected
// class. A trace grader is a deterministic ordered-signal check, kept distinct only for the priority record.
func GradeCase(c EvalCase, o Outcome) Grade {
	pass, detail := signalsMatch(c.Expect, o.Signals)
	if c.Grader == GradeCost && pass && o.Cost > c.Budget {
		pass, detail = false, "cost exceeded budget"
	}
	score := 0.0
	if pass {
		score = 1.0
	}
	return Grade{
		CaseID:        c.ID,
		Kind:          c.Grader,
		Authoritative: c.Grader.deterministic(),
		Pass:          pass,
		Score:         score,
		Security:      c.isSecurityCase(),
		Detail:        detail,
	}
}

// signalsMatch reports whether the candidate reproduced every reference signal exactly. A missing or wrong
// signal fails, with the first divergence named (sorted for a stable message).
func signalsMatch(expect, got map[string]bool) (bool, string) {
	keys := make([]string, 0, len(expect))
	for k := range expect {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		g, ok := got[k]
		if !ok {
			return false, "missing signal: " + k
		}
		if g != expect[k] {
			return false, "wrong signal: " + k
		}
	}
	return true, ""
}
