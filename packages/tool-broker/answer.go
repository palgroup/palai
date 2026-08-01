package toolbroker

import (
	"errors"
	"fmt"
)

// THE DISTINCTION THIS FILE DRAWS, AND WHY IT IS A TYPE RATHER THAN A RULE.
//
// A tool's Exec returns one `error` value for two entirely different events, and until this type existed
// the orchestrator could not tell them apart, so it treated both as the worse one: it returned the error
// UP out of dispatchTool, the attempt died with the tool_call row still `executing`, the next attempt's
// ledger consult classified that row `uncertain`, and the reconciler escalated it to `manual_resolution`
// — where nothing ever resolves it. A run that asked to read a file that is not there sat `running`
// forever, and so did a run whose traversal was correctly REFUSED.
//
// The two events are:
//
//   - AN ANSWER. The model asked for something impossible and the tool is telling it so: no such file,
//     permission denied, the path escapes the workspace, argv is not an array, this tool is not wired on
//     this deployment. NOTHING HAPPENED — no effect landed, no state is ambiguous — and the honest thing
//     is to hand the model the refusal in the same shape a success comes back in, so it can correct
//     itself on the next turn. A coding agent guessing a filename is the most ordinary thing a coding
//     agent does; it must not be a fatal event.
//
//   - A FAULT. The machinery that runs tools broke, or an effect may have half-landed: a lost database
//     connection, a fence violation, an output that failed its schema after the side effect already ran.
//     These are NOT answers. Turning one into a tool result would let a poisoned attempt keep talking and
//     would swallow a fence violation, which is worse than the wedge this file exists to remove.
//
// SO THE SAFE DIRECTION IS THE DEFAULT, and that is the whole reason this is an opt-in wrapper rather
// than a classifier over error text: an error nobody has classified is a FAULT and still aborts the
// attempt exactly as it did before. Widening the answer set is a deliberate edit to a tool, reviewable
// one call site at a time; it can never happen by accident because somebody's error message changed.

// ErrToolAnswer is the sentinel every answer error matches. It is the same idiom the run loop already
// uses for errToolUncertainWait and errRunParked — a sentinel on a switch, so the two ends agree without
// parsing each other's text (spec §26.7's shape, applied one layer down).
var ErrToolAnswer = errors.New("tool_answer")

// The shared answer codes. A code is what the MODEL keys on, so it is a short stable string rather than
// a sentence, and it lives here rather than in each tool so two tools cannot spell the same refusal two
// ways. A tool may pass its own code; these are the ones more than one tool needs.
const (
	// AnswerNotFound: the named thing does not exist.
	AnswerNotFound = "not_found"
	// AnswerInvalidArguments: the arguments are malformed or fail their schema. Nothing ran.
	AnswerInvalidArguments = "invalid_arguments"
	// AnswerUnknownTool: no tool of that name is resolvable for this run.
	AnswerUnknownTool = "unknown_tool"
	// AnswerRefused: the tool understood the request and declined it — a traversal, a likely-secret read,
	// a write to a read-only workspace. THE REFUSAL WORKED; this is what it sounds like from the model's
	// side, and it must not also take the run down.
	AnswerRefused = "refused"
	// AnswerUnavailable: the tool is not wired on this deployment (no shell posture, no workspace bound,
	// no registry). The model cannot fix it, but being told beats a run that never ends — and the
	// tool_calls row records the refusal for the operator who can fix it.
	AnswerUnavailable = "unavailable"
	// AnswerFailed: the operation failed for a reason none of the above names. Reserved for surfaces
	// where FAILING CHANGED NOTHING (a read), never for one where an effect may have landed.
	AnswerFailed = "failed"
)

// AnswerError carries a tool's answer-shaped refusal: a stable code the model keys on and the cause,
// which stays wrapped so `errors.Is(err, fs.ErrNotExist)` keeps working through it.
type AnswerError struct {
	Code string
	Err  error
}

func (e *AnswerError) Error() string { return e.Err.Error() }

// Unwrap exposes the CAUSE, so a caller can still ask what kind of failure it was.
func (e *AnswerError) Unwrap() error { return e.Err }

// Is matches the ErrToolAnswer sentinel. Together with Unwrap this makes one value answer both
// questions — "is this an answer?" and "an answer to what?" — from the one errors.Is chain.
func (e *AnswerError) Is(target error) bool { return target == ErrToolAnswer }

// Answer wraps a cause as the tool's answer to the model under a stable code. A nil cause is a
// programming error and yields nil rather than an answer with nothing in it.
func Answer(code string, err error) error {
	if err == nil {
		return nil
	}
	if code == "" {
		code = AnswerFailed
	}
	return &AnswerError{Code: code, Err: err}
}

// Answerf is Answer over a formatted message, for a refusal a tool states itself rather than relaying.
func Answerf(code, format string, a ...any) error {
	return &AnswerError{Code: code, Err: fmt.Errorf(format, a...)}
}

// AsAnswer reports whether err is (or wraps) an answer, and returns it. It is the ONE test the
// dispatcher makes: anything this returns false for aborts the attempt, unchanged from before this
// type existed.
func AsAnswer(err error) (*AnswerError, bool) {
	var answer *AnswerError
	if errors.As(err, &answer) {
		return answer, true
	}
	return nil, false
}
