package execution

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/extensions"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
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
	// The per-attempt sandbox context every tool resolution runs through. Hoisted above the consult
	// because the approval gate resolves the tool through the SAME lookup the executor uses — the
	// identity a human authorizes has to be the identity that runs, so it cannot come from the frame.
	env := o.execEnv(st)
	// approved marks a call a human already said yes to (the ledger row is `ready`). It is the one state
	// that RE-ENTERS the execute path from the consult rather than returning from it.
	approved := false

	// 1. Durable consult (cross-kill dedup + uncertain block + the approval states, §26.7).
	state, stored, storedClass, _, storedHash, found, err := o.spine.LookupToolCall(ctx, st.tenant, callID)
	if err != nil {
		return err
	}
	if found {
		switch state {
		case "approval_pending":
			// A human still owes an answer. This attempt replayed the transcript back to the same request;
			// park again rather than asking a second time — the first park is authoritative, so the button
			// already in front of a human stays the live one.
			return o.parkForApproval(ctx, st)
		case "ready":
			// A human approved THESE BYTES. The binding is checked before anything runs: if the request
			// hash on the row is not this request's, the approval that exists authorizes a different call.
			// Refusing loudly rather than politely is the TOL-016 shape, and it is the right one here: a
			// same-id call whose content moved between the question and the answer is not a case for a
			// gentler denial, it is a case for nothing happening and somebody looking.
			if storedHash != "" && storedHash != requestHash {
				return fmt.Errorf("tool_call %q diverged after approval (hash %s != approved %s): a human approved different arguments, so this call has no approval", callID, requestHash, storedHash)
			}
			approved = true
		case "canceled":
			// Denied by a human, expired, or refused by a hook after the fact. The answer was written to
			// the ledger row when the decision was made — durable before it is spoken — so deliver it and
			// let the run continue. Same delivery path as a before_tool deny; no second one exists.
			return o.deliverToolResult(ctx, st, frame, callID, stored, false)
		case "completed", "reconciled_completed", "reconciled_not_applied":
			// A committed row is recognised BY CONTENT (spec §26.7, TOL-016): the same tool_call_id must
			// carry the same (name, args). A diverged repeat is a protocol violation — replaying the stored
			// result would answer a DIFFERENT call with the wrong content — so fail rather than mislead.
			if storedHash != "" && storedHash != requestHash {
				return fmt.Errorf("tool_call %q replayed with diverged content (hash %s != %s): same id, different request", callID, requestHash, storedHash)
			}
			// Replay the committed result LABELED; no re-execute, no re-commit (§26.7, TOL-001/016). before_tool
			// (effect-scoped) does NOT re-fire — the effect already ran. after_tool (delivery-scoped) DOES
			// re-fire here: it acts on the DELIVERED view, so a crash between an after_tool deny/redact and the
			// engine consuming the result must not un-deny it. The committed row is untouched (raw result).
			return o.deliverToolResultWithAfterTool(ctx, st, frame, callID, name, stored, true)
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

	// 1b. before_tool hooks fire AFTER the ledger consult, BEFORE any pre-write/Execute (spec §28.17, the
	// critical seam pin). A committed-replay row already returned above, so a replay NEVER re-fires hooks
	// (bit-unchanged). requestHash was computed from the MODEL'S ORIGINAL args and is untouched — a transform
	// changes only what Execute runs and what the ledger records, never the replay/divergence identity. A
	// policy DENY leaves NO executing/pre-write row: it journals policy.denied.v1 and delivers a structured
	// deny result the model sees and continues on.
	execArgs := args
	outcome0, err := o.fireHook(ctx, st, extensions.HookPointBeforeTool, map[string]any{"tool_name": name, "arguments": args})
	if err != nil {
		return err
	}
	if outcome0.Denied {
		// A deny on a re-driven executing/leased row (a kill-interrupted pure/idempotent call that fell through
		// the consult above) must NOT leave the row 'executing' forever — nothing reconciles a non-uncertain
		// executing row, and the effect may have applied once. Mark it uncertain so the reconciler resolves it;
		// end the attempt like any uncertain block. A FRESH call (no row) delivers the structured denial below.
		//
		// An APPROVED row is NOT that case and must not be marked uncertain: nothing ran, so nothing is
		// uncertain. It takes the fresh-call exit below — the structured denial — and the row is left
		// `ready`. Leaving it is the honest state: a human's approval is permission, never an override, so
		// a policy that says no keeps saying no on every replay, and the call runs only if the policy stops
		// refusing. ponytail: no cancel transition for this path, because a `ready` row is inert until a
		// hook lets it through, which is exactly the outcome that should let it through.
		if found && !approved {
			if _, err := o.spine.MarkToolCallUncertain(ctx, st.tenant, st.sessionID, st.responseID, runID, callID); err != nil {
				return err
			}
			return errToolUncertainWait
		}
		if err := o.journalPolicyDenied(ctx, st, extensions.HookPointBeforeTool, outcome0.HookID, outcome0.Reason,
			map[string]any{"tool_call_id": callID, "tool_name": name}); err != nil {
			return err
		}
		denial, _ := json.Marshal(map[string]any{"status": "denied", "reason": outcome0.Reason, "hook_id": outcome0.HookID})
		return o.deliverToolResult(ctx, st, frame, callID, string(denial), false)
	}
	if patched, ok := outcome0.Payload["arguments"].(map[string]any); ok {
		// A transform patch changes ONLY the args Execute runs with AND the ledger `arguments` column — honest
		// audit of what actually executed. The requestHash/identity above stays the model's original.
		execArgs = patched
	}

	// 2. Pre-write a side-effecting tool BEFORE the effect so a kill is detectable as uncertain (§26.7).
	// Pure/idempotent skip it (re-run/resend is safe). Skipped when a row already exists (a re-executing
	// pure row, or a fence re-lease) — BeginToolCall is idempotent, but avoiding it keeps the fresh path lean.
	// The ledger records the PATCHED args (what actually runs), not the model's original.
	arguments, _ := json.Marshal(execArgs)
	// Resolve the class through the SAME lookup the executor uses, so a registered registry tool's DECLARED
	// class (e.g. irreversible) drives the pre-write marker — not the ClassPure static-miss default (M2).
	class, err := o.tools.ReplayClassResolved(ctx, env, name)
	if err != nil {
		return err
	}

	// 2a. THE APPROVAL GATE (E23 T1, spec §22.4). It sits HERE — after before_tool, before the pre-write —
	// for a reason worth stating: a policy hook that already refuses this call has refused it, and there is
	// no sense interrupting a human to ask about something the deployment has already said no to. A human's
	// attention is the scarcest thing this system spends.
	//
	// It engages only on a FRESH call. A row that exists has already been through the gate: it is parked,
	// approved, canceled, or was never gated at all, and the consult above resolved which.
	if !found {
		// The label is deliberately DISCARDED here. It belongs to the same row as the gate — which is why
		// one lookup returns both — but the dispatcher does not render anything, and storing it now would
		// be the stored display this design refuses. Each surface resolves it again at render time, from
		// the same lookup, and gets the same answer.
		required, _, aerr := o.tools.RequiresApprovalResolved(ctx, env, name)
		if aerr != nil {
			return aerr
		}
		if required {
			approvalID := newExecID("apr")
			// The row records the args that will RUN (the patched set, if a transform fired) and the hash of
			// the model's original — the same split the pre-write uses, so identity and audit keep meaning
			// exactly what they mean everywhere else. The button binds to the hash; the screen shows the args.
			if err := o.spine.RequestToolApproval(ctx, st.tenant, coordinator.ToolApprovalRequest{
				ApprovalID: approvalID, ToolCallID: callID, SessionID: st.sessionID, ResponseID: st.responseID,
				RunID: runID, Fence: st.attempt.Fence, ToolName: name, Arguments: arguments,
				ReplayClass: string(class), RequestHash: requestHash,
				ExpiresAt: time.Now().Add(coordinator.ApprovalTTL()), Boundary: st.lastModelRequestID,
			}); err != nil {
				return err
			}
			return o.parkForApproval(ctx, st)
		}
	}

	// An APPROVED call must be pre-written whatever its class. The consult left the row at `ready`, and
	// CommitToolResult only advances executing/leased — a `ready` row would change 0 rows and be reported a
	// stale commit, so the result of a call a human authorized would be thrown away. It is also the honest
	// marker: an approved call is by definition one whose effect somebody wanted looked at, so a kill
	// between execute and commit must be detectable as uncertain rather than as "never ran".
	if (toolbroker.NeedsPreWrite(class) && !found) || approved {
		// TOL-017's fence half is the CommitToolResult fence guard; the async-callback transport half keys
		// on the durable pre-write (E12 T4). external_idempotency_key = tool_call_id ONLY for a tool that
		// sends a real external Idempotency-Key (the remote_http invoke); a built-in records none (honest).
		// commit_boundary = the model_request_id that produced this call, so a late-callback reconcile knows
		// the boundary it belongs to.
		externalKey := ""
		keyed, err := o.tools.ExternalKeyedResolved(ctx, env, name)
		if err != nil {
			return err
		}
		if keyed {
			externalKey = callID
		}
		if err := o.spine.BeginToolCall(ctx, st.tenant, st.sessionID, st.responseID, runID, st.attempt.Fence,
			callID, name, arguments, string(class), requestHash, externalKey, st.lastModelRequestID); err != nil {
			return err
		}
	}

	// 2b. WHAT RUNS IS WHAT WAS SHOWN. An approved call executes the arguments off its OWN ledger row —
	// the bytes the approval screen was derived from and the button was bound to — rather than whatever
	// this attempt's frame carries or this attempt's transform hook produced. The hash guard in the
	// consult already refuses a diverged frame; this closes the quieter half, where the frame agrees but a
	// non-deterministic transform would have substituted something the human never read.
	if approved {
		parked, ok, perr := o.spine.ToolApprovalForCall(ctx, st.tenant, callID)
		if perr != nil {
			return perr
		}
		if !ok {
			return fmt.Errorf("tool_call %q is ready to run but its approval row is gone: refusing to execute an authorization that cannot be read", callID)
		}
		var approvedArgs map[string]any
		if err := json.Unmarshal(parked.Arguments, &approvedArgs); err != nil {
			return fmt.Errorf("decode approved arguments for %q: %w", callID, err)
		}
		execArgs = approvedArgs
		arguments = parked.Arguments
	}

	// THE LAST MOMENT (E25 T3). The attempt's environment VALUES are resolved here and nowhere earlier:
	// after the durable consult, after BOTH approval parks (`approval_pending` above and the fresh-call gate
	// at :183, both of which RETURN), after the before_tool hook, one statement before the call that uses
	// them. So a run waiting for a human holds no credential in memory while it waits — the park left this
	// function before this line was reached.
	//
	// The map's whole life is the Execute call below. `env` is a per-dispatch local (execEnv mints a fresh
	// value each call), it is not read again after :249, and it is never marshalled, journaled or logged:
	// the four resolver lookups above take `env` BEFORE this line, so none of them can see a value either.
	if env.EnvValues, err = o.resolveEnvValues(ctx, st); err != nil {
		return err
	}

	// 3. Execute + commit + deliver. Execute runs the PATCHED args (a transform hook's replacement, or the
	// model's original when no transform fired), so the ledger row and the effect agree (honest audit).
	outcome, err := o.tools.Execute(ctx, contracts.ToolCallID(callID), name, execArgs, st.attempt.Fence, env)
	if err != nil {
		return fmt.Errorf("execute tool %q (%s): %w", name, callID, err)
	}
	st.usage = addUsage(st.usage, outcome.Usage)

	result, _ := json.Marshal(outcome.Result)
	// AN UNRETAINED TOOL'S OUTPUT IS NEVER WRITTEN DOWN (E21 T5, §3.5 M5). The ledger row still exists —
	// the call happened, the fence advanced, an auditor can see WHAT was asked and WHEN — but the bytes it
	// returned are replaced by a marker. The engine below is still delivered the REAL result: the vendor's
	// term restricts STORING the data, not answering with it.
	//
	// The two consequences are deliberate and neither is silent. A committed-replay after a kill serves this
	// marker rather than the results, which is the honest answer for a search whose action_token did not
	// survive the restart either; and an operator reading the ledger sees that a search ran and that its
	// content was not kept, rather than seeing nothing at all.
	committedResult := result
	if outcome.Unretained {
		committedResult, _ = json.Marshal(map[string]any{
			"unretained": true,
			"reason": "this tool's results may not be stored or copied (Slack Real-time Search API terms of " +
				"use); the model received them, nothing wrote them down",
		})
	}
	payload, _ := json.Marshal(map[string]any{"run_id": st.attempt.RunID, "tool_call_id": callID})
	// The ledger row carries the tool's DECLARED replay class and the model's ORIGINAL request hash (NOT
	// outcome.Hash, which a before_tool transform would recompute over the patched args): the identity a
	// redelivery re-derives from the untouched original args must match, so a transform never false-diverges a
	// replay (§26.6, the seam pin). The `arguments` column stays PATCHED (honest audit of what ran). A
	// stale-fence late callback is rejected here (TOL-017, ErrStaleToolCommit).
	if _, err := o.spine.CommitToolResult(ctx, st.tenant, st.sessionID, st.responseID, runID,
		st.attempt.Fence, callID, name, arguments, committedResult, string(outcome.ReplayClass), requestHash, toolCallCompletedEvent, payload); err != nil {
		return err
	}

	// 4. THE PUBLICATION PARK (E23 T3). A call that left a human OWING AN ANSWER is not answered to the
	// model: the run parks here, after the commit and before the delivery, so the ledger row is durable (a
	// woken attempt replays it rather than re-firing the effect) while the model is handed nothing to
	// continue on. That ordering is what makes the difference from the shipped behaviour — the tool's
	// receipt is recorded, it is simply not spoken until a human has decided.
	//
	// after_tool does NOT fire here, and that is correct rather than convenient: it is DELIVERY-scoped, and
	// nothing has been delivered. The woken attempt's consult reaches `completed`, fires it there, and the
	// model sees exactly one delivery.
	switch parked, perr := o.parkOnPendingPublication(ctx, st); {
	case perr != nil:
		return perr
	case parked:
		return errRunAwaitingApproval
	}

	// after_tool fires on the delivery path (fresh here; a committed replay re-fires it in the consult above).
	//
	// ponytail: a woken attempt replays the tool's own receipt, which still reads "pending_approval" — true
	// when it was written, stale by the time a human has decided. Binding the decision back onto the ledger
	// row (as a gated call's deny does) needs an approvals row carrying BOTH a publication and a tool call,
	// and 000044's CHECK makes those mutually exclusive by design. Worth a migration the first time an
	// operator reads "I have requested approval" under a branch that was already pushed.
	return o.deliverToolResultWithAfterTool(ctx, st, frame, callID, name, string(result), false)
}

// deliverToolResultWithAfterTool fires after_tool hooks and delivers the (possibly transformed or denied)
// VIEW to the model (spec §28.17). after_tool is DELIVERY-scoped — unlike the effect-scoped before_tool, it
// re-fires on BOTH the fresh commit path and the committed-replay path, so a crash between an after_tool
// deny/redact and the engine consuming the result can NEVER convert it into an allow or un-redact it. The
// committed ledger row + the effect are UNTOUCHED; only the delivered view is (re-)derived. rawJSON is the
// tool's committed raw result; replayed labels a result served from the ledger after a reclaim.
func (o *Orchestrator) deliverToolResultWithAfterTool(ctx context.Context, st *attemptState, frame contracts.EngineFrame, callID, name, rawJSON string, replayed bool) error {
	var rawResult any
	_ = json.Unmarshal([]byte(rawJSON), &rawResult)
	after, err := o.fireHook(ctx, st, extensions.HookPointAfterTool, map[string]any{"tool_name": name, "result": rawResult})
	if err != nil {
		return err
	}
	if after.Denied {
		if err := o.journalPolicyDenied(ctx, st, extensions.HookPointAfterTool, after.HookID, after.Reason,
			map[string]any{"tool_call_id": callID, "tool_name": name}); err != nil {
			return err
		}
		denial, _ := json.Marshal(map[string]any{"status": "denied", "reason": after.Reason, "hook_id": after.HookID})
		return o.deliverToolResult(ctx, st, frame, callID, string(denial), replayed)
	}
	// The delivered view is re-derived from the (possibly patched) payload result. Unpatched — including a
	// hook-less run — it round-trips to the same bytes (everything went through json.Marshal at commit), so
	// this is bit-unchanged; a transform substitutes its redacted view.
	delivered := rawJSON
	if patched, ok := after.Payload["result"]; ok {
		if patchedBytes, mErr := json.Marshal(patched); mErr == nil {
			delivered = string(patchedBytes)
		}
	}
	return o.deliverToolResult(ctx, st, frame, callID, delivered, replayed)
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
		Artifacts:     o.artifacts, // the research tool persists a large body here; nil ⇒ excerpt-only
		Scope: toolbroker.TaskScope{
			Org: st.tenant.Organization, Project: st.tenant.Project,
			SessionID: st.sessionID, RunID: string(st.attempt.RunID), ResponseID: st.responseID,
		},
	}
}
