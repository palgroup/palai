package execution

import (
	"fmt"

	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// The orchestrator journals these canonical event types (spec §13.3/§21.3), each already
// present in protocols/schemas/execution/event-types.json. The model-step names are owned
// here; the tool-call completion name is sourced from the state-machine table so the table
// stays the single definition of that transition's event (Step 7 fold — the ad-hoc
// run.model_request/result/tool_result.v1 names were folded onto these canonical ones).
const (
	eventModelStepCreated   = "model_step.created.v1"
	eventModelStepCompleted = "model_step.completed.v1"
	// eventModelStepInterrupted is the partial record of an interrupt-aborted provider call: a
	// user-initiated partial, NOT a failure (spec §25.11: "its outcome is recorded as partial",
	// spec §9.2 interrupt). Distinct from model_step.failed.v1 so the journal never mislabels an
	// interrupt as a failure. Its payload carries the streamed-so-far output as the explicit
	// partial item (spec §25.16), not None.
	eventModelStepInterrupted = "model_step.interrupted.v1"
	// eventConfigRevised journals a session config revision's content — the redacted, content-
	// addressed ConfigSnapshot with provenance — at the boundary it applied (spec §9.3, §14).
	eventConfigRevised = "config.revised.v1"
	// eventWarningRaised marks an immediate config switch that interrupted the in-flight model
	// step (spec §9.3, §25.16): the aborted attempt's state may be omitted, so the switch is not
	// silently presented as a clean step boundary.
	eventWarningRaised = "warning.raised.v1"
)

// toolCallCompletedEvent is the event the tool-call table emits on Executing->Completed,
// read from the table rather than hardcoded, so the phase-02 ToolCallTable remains the
// single source of that name (tool_call.reconciled_completed.v1 is the §26 uncertain path,
// not this one).
var toolCallCompletedEvent = mustTransitionEvent(statemachines.ToolCallExecuting, statemachines.ToolCallCmdComplete)

// emittedEventTypes is every journal event type the execution package commits. The
// registry cross-check asserts this stays a subset of the canonical registry, so a future
// ad-hoc name is caught rather than silently drifting outside it.
var emittedEventTypes = []string{
	eventModelStepCreated,
	eventModelStepCompleted,
	eventModelStepInterrupted,
	eventConfigRevised,
	eventWarningRaised,
	toolCallCompletedEvent,
}

// mustTransitionEvent returns the event the tool-call table emits for a transition,
// panicking at package init if the table has no such row — a broken single source of
// truth is a build-time defect, not a runtime one.
func mustTransitionEvent(from statemachines.ToolCallState, cmd statemachines.ToolCallCommand) string {
	_, event, err := statemachines.Apply(from, cmd, statemachines.ToolCallTable)
	if err != nil {
		panic(fmt.Sprintf("tool-call table has no %v->%v transition: %v", from, cmd, err))
	}
	return event
}
