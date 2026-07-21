package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

// problemTypePrefix namespaces stable codes into dereferenceable problem types,
// matching the HTTP surface's middleware.WriteProblem so a stored terminal error and a
// live problem document share one type URI. Sourced from contracts so the prefix has one
// definition across the finalize and cancel projections.
const problemTypePrefix = contracts.ProblemTypePrefix

// terminalCommands maps an engine run.terminal outcome to its canonical run command
// and the terminal response status (spec §25.8, §22.3).
var terminalCommands = map[string]struct {
	command statemachines.RunCommand
	status  string
}{
	"completed":       {statemachines.RunCmdComplete, "completed"},
	"failed":          {statemachines.RunCmdFail, "failed"},
	"canceled":        {statemachines.RunCmdCancel, "canceled"},
	"timed_out":       {statemachines.RunCmdTimeout, "timed_out"},
	"budget_exceeded": {statemachines.RunCmdExhaustBudget, "budget_exceeded"},
}

// terminalProblems maps a non-completed terminal status to the sanitized RFC 9457
// problem the Response projection carries as its error (spec §22.3, §8.3). Each detail
// is a fixed human line — never raw provider or engine text. request_id is stamped at
// retrieval, not here, since a terminal is finalized off any HTTP request. canceled is
// NOT here — it is the single contracts.CanceledProblem the endpoint cancel and this
// engine-terminal path share (the ledger's canonical-problem dedup).
var terminalProblems = map[string]contracts.Problem{
	"failed":          {Code: "internal_error", Title: "Internal error", Status: 500, Detail: "the run failed during execution", Retryable: true},
	"timed_out":       {Code: "operation_timed_out", Title: "Operation timed out", Status: 504, Detail: "the run exceeded its execution deadline", Retryable: true},
	"budget_exceeded": {Code: "quota_exceeded", Title: "Quota exceeded", Status: 429, Detail: "the run exhausted its allotted budget"},
}

// terminalProblem returns the sanitized problem a non-completed terminal projects as
// its error, or nil for a completed run (which carries no error). canceled projects the
// single canonical contracts.CanceledProblem so the endpoint-cancel and engine-terminal
// projections stay one document; the rest derive their type URI from the stable code.
func terminalProblem(status string) *contracts.Problem {
	if status == "canceled" {
		p := contracts.CanceledProblem()
		return &p
	}
	p, ok := terminalProblems[status]
	if !ok {
		return nil
	}
	p.Type = problemTypePrefix + p.Code
	return &p
}

// finalize handles run.terminal: it applies exactly one terminal run transition and
// writes the terminal Response projection from the committed run, output, and usage.
func (o *Orchestrator) finalize(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	outcome, _ := frame.Data["outcome"].(string)
	terminal, ok := terminalCommands[outcome]
	if !ok {
		return fmt.Errorf("engine terminal frame has unknown outcome %q", outcome)
	}

	// Exactly one terminal transition, and exactly one terminal projection. A run that is
	// already terminal was finalized by whoever won the transition (a completed engine, or a
	// user cancel that raced this in-flight attempt), so a late or duplicate run.terminal must
	// NOT overwrite that projection — the response UPDATE is unconditional, and a completed
	// terminal landing on a canceled run would surface a second terminal (§22.3). Skip the
	// write, mirroring the coordinator's dead-letter sweep (lease.go).
	switch _, err := o.spine.ApplyRunTransition(ctx, st.tenant, string(st.attempt.RunID), terminal.command); {
	case errors.Is(err, coordinator.ErrRunTerminal):
		return nil
	case errors.Is(err, statemachines.ErrInvalidState):
		// A non-terminal state that cannot take this command still refreshes the projection below.
	case err != nil:
		return err
	}

	// Compile the run's changeset from the tool ledger while the workspace is still on disk and the
	// writer lease still held (spec §30.6, E09 Task 10): the changed-file set + patch + test log come
	// from the file/shell tool_calls the run issued, NOT model prose (REP-005). This is the exact call
	// the changeset.go deferral named — now auto-invoked. It is a clean no-op when no artifact writer is
	// wired or the run bound no workspace, and CompileChangeset itself skips a run that prepared no repo.
	//
	// A compile error (e.g. an S3 hiccup) is LOGGED, not fatal: the run has already transitioned to its
	// terminal state above, so failing the attempt would only bail on the already-terminal run on retry
	// (never recompiling) while dropping the response projection. The changeset is REP-005 evidence
	// recomputable from the immutable ledger (E10 replay), so a completed run is not blocked on it.
	if o.artifacts != nil && st.attempt.WorkspaceHostPath != "" {
		if _, _, err := CompileChangeset(ctx, o.spine, o.artifacts, ChangesetInput{
			Tenant:         st.tenant,
			SessionID:      st.sessionID,
			ResponseID:     st.responseID,
			RunID:          string(st.attempt.RunID),
			AllocationRoot: st.attempt.WorkspaceHostPath,
		}); err != nil {
			log.Printf("compile changeset for run %s: %v", st.attempt.RunID, err)
		}
	}

	output := st.output
	if len(output) == 0 {
		if value, ok := frame.Data["output"]; ok && value != nil {
			output = []contracts.ContentItem{{"type": "message", "content": value}}
		}
	}
	proj := map[string]any{"output": output, "usage": st.usage, "model": st.model}
	// A run that delegated identifies its ChildRuns in the terminal projection (spec §25.19): the
	// parent's final output links the child run ids, not a hidden transcript.
	if len(st.childRunIDs) > 0 {
		proj["child_runs"] = st.childRunIDs
	}
	if problem := terminalProblem(terminal.status); problem != nil {
		proj["error"] = problem
	}
	projection, err := json.Marshal(proj)
	if err != nil {
		return fmt.Errorf("marshal response projection: %w", err)
	}
	if err := o.spine.FinalizeResponse(ctx, st.tenant, st.responseID, terminal.status, projection); err != nil {
		return err
	}

	// A terminal CHILD wakes its detached parent (spec §25.18-19, E10 T8 DET-001): if this run has a
	// parent released to waiting and no non-terminal sibling remains, WakeParentOfChild re-enters the
	// parent and enqueues its response.run job — single-winner, so a redelivered terminal wakes it once.
	// A no-op for a root run (no parent) or an inline child (its parent never released). Best-effort:
	// the run's own terminal already committed, so a wake hiccup is logged, not fatal — the parent's own
	// post-release self-wake and job reclaim are the backstops.
	if _, err := o.spine.WakeParentOfChild(ctx, st.tenant, string(st.attempt.RunID)); err != nil {
		log.Printf("wake detached parent of child %s: %v", st.attempt.RunID, err)
	}
	return nil
}
