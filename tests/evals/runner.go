package evals

// SuiteReport is the runner's output for one suite+split under one policy: the pass count, the aggregate
// score (Passed/Total), the SecurityFailures tally (failed security cases — the gate's independent block,
// §57.13), and the dataset Digest (the content address that pins WHICH fixtures produced these numbers).
type SuiteReport struct {
	Suite            string
	Split            Split
	Total            int
	Passed           int
	Score            float64
	SecurityFailures int
	Digest           string
	Grades           []Grade
}

// RunSuite runs the reference engine (under policy) over a loaded revision and grades every case. The
// aggregate Score and the SecurityFailures tally are what the release gate reads for the held-out split.
func RunSuite(d DatasetRevision, policy Policy) SuiteReport {
	rep := SuiteReport{Suite: d.Suite, Split: d.Split, Total: len(d.Cases), Digest: d.Digest()}
	for _, c := range d.Cases {
		g := GradeCase(c, Solve(c, policy))
		rep.Grades = append(rep.Grades, g)
		if g.Pass {
			rep.Passed++
		} else if g.Security {
			rep.SecurityFailures++
		}
	}
	if rep.Total > 0 {
		rep.Score = float64(rep.Passed) / float64(rep.Total)
	}
	return rep
}

// RunAll loads and runs every one of the four suites for a split under a policy. It is the harness's
// end-to-end entry: reference engine -> grader -> per-suite report, the chain the gate proof drives.
func RunAll(root string, split Split, policy Policy) (map[string]SuiteReport, error) {
	out := make(map[string]SuiteReport, len(Suites))
	for _, suite := range Suites {
		d, err := LoadSuite(root, suite, split)
		if err != nil {
			return nil, err
		}
		out[suite] = RunSuite(d, policy)
	}
	return out, nil
}
