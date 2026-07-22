package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// errToolUncertainWait ends an attempt cleanly when a tool call is uncertain (spec §26.7 last sentence):
// its result would feed reasoning, so the run must NOT continue on it. dispatchTool returns this instead
// of sending tool.result; ExecuteAttempt ends the attempt (no engine hang) and the reconcile job
// resolves the row + re-enqueues the run. Not a failure — like errRunPaused.
var errToolUncertainWait = errors.New("tool_uncertain_wait")

// dispatchTool handles a tool.request behind the durable tool ledger (spec §24.7, §26.6-26.7). It first
// CONSULTS the ledger: a committed row replays the cached result LABELED without re-firing the effect
// (cross-kill dedup, TOL-001/016); an `uncertain` row blocks continuation (uncertain-STOP); an
// `executing` row left by a kill is classified — irreversible/reversible/interactive enter `uncertain`
// and STOP, pure/idempotent re-execute safely. For a fresh side-effecting call it writes a durable
// 'executing' marker BEFORE the effect (so a kill is detectable), executes, commits completed, and only
// then delivers tool.result (commit-before-deliver).
func (o *Orchestrator) dispatchTool(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	callID, _ := frame.Data["tool_call_id"].(string)
	name, _ := frame.Data["name"].(string)
	args, _ := frame.Data["arguments"].(map[string]any)
	runID := string(st.attempt.RunID)
	requestHash := toolbroker.RequestHash(name, args)

	// 1. Durable consult (cross-kill dedup + uncertain block).
	state, stored, storedClass, _, storedHash, found, err := o.spine.LookupToolCall(ctx, st.tenant, callID)
	if err != nil {
		return err
	}
	if found {
		switch state {
		case "completed", "reconciled_completed", "reconciled_not_applied":
			// A committed row is recognised BY CONTENT (spec §26.7, TOL-016): the same tool_call_id must
			// carry the same (name, args). A diverged repeat is a protocol violation — replaying the stored
			// result would answer a DIFFERENT call with the wrong content — so fail rather than mislead.
			if storedHash != "" && storedHash != requestHash {
				return fmt.Errorf("tool_call %q replayed with diverged content (hash %s != %s): same id, different request", callID, requestHash, storedHash)
			}
			// Replay the committed result LABELED; no re-execute, no re-commit (§26.7, TOL-001/016).
			return o.deliverToolResult(ctx, st, frame, callID, stored, true)
		case "uncertain", "manual_resolution":
			// §26.7 last sentence: an uncertain result blocks continuation — end the attempt, do not
			// deliver. The reconcile job resolves it and re-enqueues the run.
			return errToolUncertainWait
		case "executing", "leased":
			// A kill left this in-flight. A class that must not silently re-run enters uncertain and
			// STOPS; pure/idempotent fall through to a safe (re-)execute. ponytail: the content-divergence
			// hash guard above covers only COMMITTED replays — a pure/idempotent executing-row re-drive
			// with diverged args re-executes unchecked (harmless for pure; idempotent settles one object).
			// Add a hash check here if a diverged executing re-drive ever needs rejecting.
			if toolbroker.BlocksReplayAfterKill(toolbroker.ReplayClass(storedClass)) {
				if _, err := o.spine.MarkToolCallUncertain(ctx, st.tenant, st.sessionID, st.responseID, runID, callID); err != nil {
					return err
				}
				return errToolUncertainWait
			}
		}
	}

	// 2. Pre-write a side-effecting tool BEFORE the effect so a kill is detectable as uncertain (§26.7).
	// Pure/idempotent skip it (re-run/resend is safe). Skipped when a row already exists (a re-executing
	// pure row, or a fence re-lease) — BeginToolCall is idempotent, but avoiding it keeps the fresh path lean.
	arguments, _ := json.Marshal(args)
	// Resolve the class through the SAME lookup the executor uses, so a registered registry tool's DECLARED
	// class (e.g. irreversible) drives the pre-write marker — not the ClassPure static-miss default (M2).
	env := o.execEnv(st)
	class, err := o.tools.ReplayClassResolved(ctx, env, name)
	if err != nil {
		return err
	}
	if toolbroker.NeedsPreWrite(class) && !found {
		// external_idempotency_key + commit_boundary are left empty: TOL-017's fence half is real (the
		// CommitToolResult fence guard), but the async-callback transport half that would key on
		// commit_boundary is a signed-transport/SDK concern (E12) — no async-callback tool exists yet.
		if err := o.spine.BeginToolCall(ctx, st.tenant, st.sessionID, st.responseID, runID, st.attempt.Fence,
			callID, name, arguments, string(class), requestHash, "", ""); err != nil {
			return err
		}
	}

	// 3. Execute + commit + deliver.
	outcome, err := o.tools.Execute(ctx, contracts.ToolCallID(callID), name, args, st.attempt.Fence, env)
	if err != nil {
		return fmt.Errorf("execute tool %q (%s): %w", name, callID, err)
	}
	st.usage = addUsage(st.usage, outcome.Usage)

	result, _ := json.Marshal(outcome.Result)
	payload, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "tool_call_id": callID})
	// The ledger row carries the tool's DECLARED replay class and the canonical request hash, so a
	// kill-after-execute row is classified and a duplicate tool_call_id is recognised by content (§26.6,
	// TOL-016). A stale-fence late callback is rejected here (TOL-017, ErrStaleToolCommit).
	if _, err := o.spine.CommitToolResult(ctx, st.tenant, st.sessionID, st.responseID, runID,
		st.attempt.Fence, callID, name, arguments, result, string(outcome.ReplayClass), outcome.Hash, toolCallCompletedEvent, payload); err != nil {
		return err
	}
	return o.deliverToolResult(ctx, st, frame, callID, string(result), false)
}

// deliverToolResult sends the tool.result frame to the engine (spec §25.9): the structured result is
// serialized to a JSON string the engine hands the model as text. replayed marks a result served from
// the durable ledger after a reclaim rather than a fresh execution (§26.7, TOL-001) — an honest label,
// not a re-fire.
func (o *Orchestrator) deliverToolResult(ctx context.Context, st *attemptState, frame contracts.EngineFrame, callID, result string, replayed bool) error {
	data := map[string]any{"tool_call_id": callID, "content": result}
	if replayed {
		data["replayed"] = true
	}
	return st.ch.Send(ctx, o.frame(st, "tool.result", data, string(frame.ID)))
}

// execEnv is the per-attempt sandbox context the broker hands a workspace-touching tool: the
// allocation root every path confines to, whether this attempt holds a read-only snapshot, and the
// shell runner. A workspace-less attempt (no host path) yields a zero root, so a workspace tool
// fails cleanly instead of touching the control plane's own filesystem.
func (o *Orchestrator) execEnv(st *attemptState) toolbroker.ExecEnv {
	return toolbroker.ExecEnv{
		WorkspaceRoot: st.attempt.WorkspaceHostPath,
		ReadOnly:      st.attempt.WorkspaceReadOnly,
		Shell:         o.shell,
		Tasks:         o.tasks,
		Publications:  o.publications,
		Scope: toolbroker.TaskScope{
			Org: st.tenant.Organization, Project: st.tenant.Project,
			SessionID: st.sessionID, RunID: string(st.attempt.RunID), ResponseID: st.responseID,
		},
	}
}
