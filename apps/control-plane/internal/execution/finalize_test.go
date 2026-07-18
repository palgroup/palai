package execution

import "testing"

// terminalProblem is what carries the error into a non-completed run's Response projection.
// A key drift here (a renamed status or code) would silently drop the error from the GET
// projection, so pin the mapping: every non-completed terminal maps to a non-nil problem
// with the right stable code and a dereferenceable type, and a completed run maps to nil.
func TestTerminalProblem(t *testing.T) {
	for status, wantCode := range map[string]string{
		"failed":          "internal_error",
		"timed_out":       "operation_timed_out",
		"budget_exceeded": "quota_exceeded",
		"canceled":        "canceled",
	} {
		problem := terminalProblem(status)
		if problem == nil {
			t.Fatalf("terminalProblem(%q) = nil, want a problem with code %q", status, wantCode)
		}
		if problem.Code != wantCode {
			t.Errorf("terminalProblem(%q).Code = %q, want %q", status, problem.Code, wantCode)
		}
		if problem.Type != problemTypePrefix+wantCode {
			t.Errorf("terminalProblem(%q).Type = %q, want %q", status, problem.Type, problemTypePrefix+wantCode)
		}
	}

	if problem := terminalProblem("completed"); problem != nil {
		t.Errorf("terminalProblem(\"completed\") = %+v, want nil (a completed run carries no error)", problem)
	}
}
