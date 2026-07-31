package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
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

// queueTimeoutProjection is the terminal Response body a §20.12 queue-deadline timeout finalizes to:
// empty output/usage and the canonical timed_out problem. Built once from terminalProblem so a
// queue-timed-out run carries the same error document as an engine-terminal timed_out run.
var queueTimeoutProjection = mustQueueTimeoutProjection()

func mustQueueTimeoutProjection() []byte {
	body, err := json.Marshal(map[string]any{
		"output": []contracts.ContentItem{},
		"usage":  contracts.Usage{},
		"error":  terminalProblem("timed_out"),
	})
	if err != nil {
		panic(fmt.Sprintf("marshal queue-timeout projection: %v", err))
	}
	return body
}

// capacityTimeoutProjection is the terminal Response body a §T5 park-TTL expiry finalizes to (E24 T5): the
// canonical timed_out problem with ONE line replaced.
//
// THE DETAIL IS THE POINT OF HAVING A SECOND PROJECTION AT ALL. The queue-deadline body reads "the run
// exceeded its execution deadline", which for a run that never started is not just imprecise, it sends its
// owner looking for a slow model. This one names what actually happened — no machine joined the pool — so
// the answer a caller reads is the answer they need, which is the whole of §T5's requirement that the
// no-capacity outcome be LEARNED rather than silently died of. Everything else is shared: same stable code,
// same status, same type URI, so it is one document class with one honest sentence.
var capacityTimeoutProjection = mustCapacityTimeoutProjection()

func mustCapacityTimeoutProjection() []byte {
	problem := terminalProblem("timed_out")
	problem.Detail = "no runner joined this run's pool before the fleet park deadline, so the run never started"
	body, err := json.Marshal(map[string]any{
		"output": []contracts.ContentItem{},
		"usage":  contracts.Usage{},
		"error":  problem,
	})
	if err != nil {
		panic(fmt.Sprintf("marshal capacity-timeout projection: %v", err))
	}
	return body
}

// advanceResponse drives one ResponseTable transition on a run's response (spec §8.3, E26 T3), and
// tolerates the two outcomes that are not failures.
//
// A RUN WITH NO RESPONSE ROW says nothing: runs.response_id is nullable and a run reached through
// recovery or a fixture may carry none, and there is no response state to advance in that case.
//
// A REFUSED COMMAND IS THE MONOTONICITY GUARANTEE, not an error to propagate. ResponseTable's terminal
// states are absorbing, so a response finalized by a cancel or by an engine terminal refuses every
// command and this returns cleanly, leaving the terminal exactly as it was found — the same rule
// UpdateResponse's terminal-excluding WHERE enforces for the projection, applied to the lifecycle.
func (o *Orchestrator) advanceResponse(ctx context.Context, tenant coordinator.Tenant, responseID string, cmd statemachines.ResponseCommand) error {
	if responseID == "" {
		return nil
	}
	switch _, err := o.spine.AdvanceResponse(ctx, tenant, responseID, cmd); {
	case errors.Is(err, statemachines.ErrInvalidState):
		return nil
	default:
		return err
	}
}

// finalize handles run.terminal: it parks the run if the model has left a background task running,
// and otherwise applies exactly one terminal run transition and writes the terminal Response
// projection from the committed run, output, and usage.
func (o *Orchestrator) finalize(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	outcome, _ := frame.Data["outcome"].(string)
	terminal, ok := terminalCommands[outcome]
	if !ok {
		return fmt.Errorf("engine terminal frame has unknown outcome %q", outcome)
	}

	// THE BACKGROUND PARK (E26 T3, §2.3). The model has said it is done; the operating system says a
	// process this run started is still running. Finishing here would strand that process — nothing
	// would fold its exit back in, because a terminal run has no next turn — so the run is RELEASED to
	// waiting instead, through the same parkRun the approval gate uses, and T4's exit notification
	// re-enters it. No second parking mechanism, because two parking paths mean two waking bugs.
	//
	// ONLY A COMPLETED TERMINAL PARKS, and that is a decision rather than an omission. A run that
	// failed, was canceled, timed out or exhausted its budget has nowhere for a notification to land:
	// parking it would leave it waiting for a wake whose whole purpose is to give the model another
	// turn, and there is no model turn left. Those terminals finalize, and what deals with the process
	// is the run's cancellation (T5).
	//
	// THE HONEST CEILING, AND IT IS A PRODUCT DECISION RATHER THAN AN OVERSIGHT — written here because
	// this is where a reader meets it. The response stays OPEN for as long as the task runs: a caller
	// polling GET /v1/responses/{id} reads `waiting_for_tool`, and an attached SSE consumer keeps its
	// connection, for the whole of a five-minute build. The alternative — finish the run now and let the
	// exit notification open a NEW response — was refused by §2 for one reason: it is a SECOND waking
	// path, and this tree has shipped the first one twice (E23 T1, E24 T4) and paid for every divergence
	// between them. One waking path with an open response beats two with closed ones.
	if outcome == "completed" && o.runHasLiveBackgroundTask(ctx, st.tenant, string(st.attempt.RunID)) {
		return o.parkRun(ctx, st, statemachines.ResponseCmdRequestTool)
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

	// on_terminal hooks fire once the run has finalized (spec §28.17, E12 T8). This point is observer-only
	// (the matrix forbids a policy/transform here — there is nothing left to deny or patch at terminal), so
	// the verdict cannot deny; a firer error is LOGGED, not fatal — the run is already terminal and its
	// projection committed, so a hook hiccup must not un-finalize it. Fire-and-forget observers return here at
	// once. No-op when no firer is wired.
	if _, err := o.fireHook(ctx, st, extensions.HookPointOnTerminal, map[string]any{"outcome": terminal.status}); err != nil {
		log.Printf("fire on_terminal hooks for run %s: %v", st.attempt.RunID, err)
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
