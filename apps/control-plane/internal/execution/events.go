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
	// The child-delegation lifecycle events the parent journal carries (spec §25.18-19): a child
	// dispatched, a dispatched child's typed terminal, and an admission-denied delegation. Only
	// these — never the child's own model steps — enter the parent's response stream.
	eventChildRequested = "child.requested.v1"
	eventChildCompleted = "child.completed.v1"
	eventChildDenied    = "child.denied.v1"
	// The recovery ladder's visible record (spec §26.3, §26.12, E10 T4). attempt.recovering.v1
	// journals the chosen rung so a transcript reconstruction is never silently labelled an exact
	// resume; checkpoint.rejected.v1 records WHY a checkpoint was not restored (incompatible/corrupt);
	// checkpoint.migrated.v1 links a v1 checkpoint to its migrated successor (ENG-011); recovery.proof.v1
	// carries the §26.12 evidence — "resumed" is never proof on its own (REC-006).
	eventAttemptRecovering  = "attempt.recovering.v1"
	eventCheckpointRejected = "checkpoint.rejected.v1"
	eventCheckpointMigrated = "checkpoint.migrated.v1"
	eventRecoveryProof      = "recovery.proof.v1"
	// eventWorkspaceRestored records a host-lost workspace recovered onto a NEW fenced allocation
	// (spec §29.7, REC-005/ENG-006, E10 T6): the logical id stayed stable, a strictly higher fence
	// appeared, and the boundary snapshot restored checksum-EQUAL (SAN-005). It is the visible signal
	// a host move + restore succeeded, distinct from the attempt.recovering.v1 the engine-loop recovery
	// records — this is the WORKSPACE half.
	eventWorkspaceRestored = "workspace.restored.v1"
	// eventHostQuarantined records a host poisoned by an allocation-destroy failure (spec §29 SAN-008,
	// E10 T6): its bytes could not be reclaimed, so no new allocation may be placed there. The doctor
	// surfaces it; an operator clears it.
	eventHostQuarantined = "host.quarantined.v1"
	// eventToolCallProgress journals an MCP tools/call's advisory progress (E12 T5). It is NOT a
	// ToolCallTable transition — like model_step.delta it advances no state machine; it is emitted through
	// the MCP progress sink (coordinator.AppendToolProgress), best-effort.
	eventToolCallProgress = "tool_call.progress.v1"
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
	eventChildRequested,
	eventChildCompleted,
	eventChildDenied,
	eventAttemptRecovering,
	eventCheckpointRejected,
	eventCheckpointMigrated,
	eventRecoveryProof,
	eventWorkspaceRestored,
	eventHostQuarantined,
	eventToolCallProgress,
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
