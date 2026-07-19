package contracts

// This file is a handwritten helper defining canonical problem documents shared across
// packages. It is stdlib-only and defines no new exported types on the generated Problem
// (the check gate compares only *.gen.go).

// ProblemTypePrefix namespaces stable problem codes into dereferenceable type URIs, matching
// the HTTP surface's middleware.WriteProblem so a stored terminal error and a live problem
// document share one type URI.
const ProblemTypePrefix = "https://docs.palai.dev/problems/"

// CanceledProblem returns the single canonical RFC 9457 problem a canceled run projects as its
// terminal error (spec §22.3, §8.3). Both cancel paths project this exact document: the
// endpoint cancel that finalizes the response projection (apps/control-plane/internal/store) and
// the engine run.terminal canceled outcome (execution/finalize.go), so a retrieval reads the
// same canceled terminal whichever path canceled the run. It is returned by value (a fresh copy
// per call) because the caller stamps a per-retrieval request_id onto it; there is no shared
// mutable state. request_id is left empty here — it is stamped at retrieval, not at finalize.
func CanceledProblem() Problem {
	return Problem{
		Type:   ProblemTypePrefix + "canceled",
		Code:   "canceled",
		Title:  "Canceled",
		Status: 409,
		Detail: "the run was canceled before completion",
	}
}
