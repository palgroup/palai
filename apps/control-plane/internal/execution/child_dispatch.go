package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// Delegation bounds (spec §25.18). ponytail: fixed here until per-project delegation config
// arrives with the E-series carve-out — the same fixed-limits pattern as defaultAttemptLimits.
const (
	// maxChildDepth is 1: recursive delegation is off by default, so a child (depth 1) may not
	// delegate further (a depth>1 request is denied, §25.18).
	maxChildDepth = 1
	// maxChildFanout bounds the children one run may dispatch, so a runaway loop cannot spawn
	// unbounded subagents.
	maxChildFanout = 4
)

// Child workspace modes (spec §29.8, §30.5), pinned in the child.request contract in E09 Task 1 and
// ENFORCED here (Task 6 — the T1 declare-only debt closes): none = no workspace (the E08 default);
// read_only = a read-only snapshot (the child cannot mutate it); isolated = an isolated
// copy-on-write worktree on the child's own branch (mutable, merged back only explicitly).
const (
	workspaceModeNone     = "none"
	workspaceModeReadOnly = "read_only"
	workspaceModeIsolated = "isolated"
)

// childWorkspace is the resolved workspace plan a delegated child runs under (spec §30.5). It is what
// provisioning realizes: read_only → a read-only snapshot (Writable false, denied a writer lease);
// isolated → a mutable worktree on agent/<session>/<run> (Writable true); none → no workspace.
type childWorkspace struct {
	Mode     string
	Writable bool
}

// resolveChildWorkspace validates and resolves a child's workspace_mode (spec §30.5). An empty mode
// is the E08 default (none); an unrecognized value is rejected — the workspace a child receives can
// never be an unknown mode. The isolated/read_only worktree is REALIZED by the repositories worktree
// mechanics (AddIsolatedWorktree / AddReadOnlyWorktree) when the child's workspace is provisioned.
// ponytail: children run inline with no workspace provisioned yet (same gap as repository preparation
// in T3) — this resolves + gates the mode; provisioning wires the realization where it lands (T9).
func resolveChildWorkspace(mode string) (childWorkspace, bool) {
	switch mode {
	case "", workspaceModeNone:
		return childWorkspace{Mode: workspaceModeNone, Writable: false}, true
	case workspaceModeReadOnly:
		return childWorkspace{Mode: workspaceModeReadOnly, Writable: false}, true
	case workspaceModeIsolated:
		return childWorkspace{Mode: workspaceModeIsolated, Writable: true}, true
	default:
		return childWorkspace{}, false
	}
}

// childSpec is one delegation the engine asked the controller to admit and dispatch — the
// child.request frame decoded (spec §25.18). Budget is the requested max_total_tokens (0 =
// unbounded request); Required marks a delegation whose failure fails the parent (SUB-003).
type childSpec struct {
	ChildRequestID string
	Role           string
	Objective      string
	Model          string
	Tools          []string
	Budget         int
	WorkspaceMode  string
	Required       bool
}

// childAdmission is the deterministic verdict on one delegation. Denied carries a stable reason
// the parent journals and the engine folds (required → fail, optional → skip); on admit,
// EffectiveBudget is the parent-intersected reservation the ChildRun runs under (0 = unbounded).
type childAdmission struct {
	Denied          bool
	Reason          string
	EffectiveBudget int
}

// admitChild is the pure delegation gate (spec §25.18): depth (recursive-off), fan-out,
// capability = child.tools ⊆ parent ∩ project, routability (project model allowlist), and budget
// = intersect with the parent's remainder. It never dispatches — the DB ChildRun follows only on
// an admit — so it is unit-tested directly. parentRemaining is meaningful only when
// parentBounded; an over-budget request is clamped to the remainder, an exhausted one denied.
func admitChild(spec childSpec, parentDepth, fanoutUsed, parentRemaining int, parentBounded bool, policy coordinator.ConfigPolicy, parentTools []string) childAdmission {
	if parentDepth+1 > maxChildDepth {
		return childAdmission{Denied: true, Reason: "depth_exceeded"}
	}
	if fanoutUsed >= maxChildFanout {
		return childAdmission{Denied: true, Reason: "fanout_exceeded"}
	}
	// Workspace mode is enforced at the gate: an unrecognized mode is rejected rather than
	// dispatched with an unknown workspace (spec §30.5; the T1 declare-only enum is enforced here).
	if _, ok := resolveChildWorkspace(spec.WorkspaceMode); !ok {
		return childAdmission{Denied: true, Reason: "invalid_workspace_mode"}
	}
	if denied := capabilityDeniedTool(spec.Tools, parentTools, policy); denied != "" {
		return childAdmission{Denied: true, Reason: "capability_denied"}
	}
	// Routability: no conforming route for the requested model (outside the project allowlist) is a
	// typed capability failure — a required delegation then fails the parent, no silent fallback.
	if !policy.AllowModel(spec.Model) {
		return childAdmission{Denied: true, Reason: "unroutable"}
	}
	// Budget: a bounded parent with nothing left cannot fund a child — deny at the bound rather
	// than dispatch a zero-budget ChildRun (SUB-004). Otherwise intersect with the remainder.
	if parentBounded && parentRemaining <= 0 {
		return childAdmission{Denied: true, Reason: "budget_exhausted"}
	}
	return childAdmission{EffectiveBudget: intersectBudget(spec.Budget, parentRemaining, parentBounded)}
}

// capabilityDeniedTool returns the first child tool outside the parent ∩ project capability, or ""
// if every requested tool is within it. An empty parentTools or project allowlist is unrestricted
// at that layer, so the intersection narrows to whichever layer actually restricts (spec §25.18).
func capabilityDeniedTool(childTools, parentTools []string, policy coordinator.ConfigPolicy) string {
	if len(parentTools) > 0 {
		for _, t := range childTools {
			if !slices.Contains(parentTools, t) {
				return t
			}
		}
	}
	return policy.DeniedTool(childTools)
}

// intersectBudget clamps a child's requested budget to the parent's remainder (spec §25.18). An
// unbounded parent passes the request through; a bounded parent caps it at whatever is left, and a
// child that requested unbounded (0) inherits exactly the remainder. A caller only reaches this on
// a positive remainder — an exhausted parent is denied before intersection.
func intersectBudget(requested, parentRemaining int, parentBounded bool) int {
	if !parentBounded {
		return requested
	}
	if requested == 0 || requested > parentRemaining {
		return parentRemaining
	}
	return requested
}

// delegationSpec is one required delegation as it rides run config and run.start (spec §25.18). It
// is the durable shape the create body carries (root run's emit list) and a ChildRun stores as its
// own spec; the engine echoes it into a child.request the controller decodes back to a childSpec.
type delegationSpec struct {
	Role          string   `json:"role,omitempty"`
	Objective     string   `json:"objective,omitempty"`
	Model         string   `json:"model,omitempty"`
	Tools         []string `json:"tools,omitempty"`
	Budget        int      `json:"budget,omitempty"`
	WorkspaceMode string   `json:"workspace_mode,omitempty"`
	Required      bool     `json:"required,omitempty"`
}

// runDelegation is a run's delegation column (spec §25.18): Emit (+ the parent Budget children
// intersect) on a root run configured to delegate, Spec on a ChildRun (its own model/budget).
type runDelegation struct {
	Emit   []delegationSpec `json:"emit,omitempty"`
	Budget int              `json:"budget,omitempty"`
	Spec   *delegationSpec  `json:"spec,omitempty"`
}

// parseRunDelegation decodes a run's delegation JSON; an empty/NULL column is the zero value (a
// plain run — no delegations to emit, no child spec).
func parseRunDelegation(raw []byte) runDelegation {
	if len(raw) == 0 {
		return runDelegation{}
	}
	var d runDelegation
	_ = json.Unmarshal(raw, &d)
	return d
}

// emitFrames renders the seeded delegations as the run.start data.delegations the engine emits as
// child.request frames. The workspace_mode default is carried in the contract; admitChild now
// validates it and resolveChildWorkspace resolves the plan (Task 6).
func (d runDelegation) emitFrames() []map[string]any {
	out := make([]map[string]any, 0, len(d.Emit))
	for _, s := range d.Emit {
		mode := s.WorkspaceMode
		if mode == "" {
			mode = "none"
		}
		out = append(out, map[string]any{
			"role": s.Role, "objective": s.Objective, "model": s.Model,
			"tools": s.Tools, "budget": map[string]any{"max_total_tokens": s.Budget},
			"workspace_mode": mode, "required": s.Required,
		})
	}
	return out
}

// decodeChildSpec reads a child.request frame's data into the childSpec the admission gate scores.
func decodeChildSpec(data map[string]any) childSpec {
	spec := childSpec{}
	spec.ChildRequestID, _ = data["child_request_id"].(string)
	spec.Role, _ = data["role"].(string)
	spec.Objective, _ = data["objective"].(string)
	spec.Model, _ = data["model"].(string)
	spec.WorkspaceMode, _ = data["workspace_mode"].(string)
	spec.Required, _ = data["required"].(bool)
	if tools, ok := data["tools"].([]any); ok {
		for _, t := range tools {
			if s, ok := t.(string); ok {
				spec.Tools = append(spec.Tools, s)
			}
		}
	}
	if budget, ok := data["budget"].(map[string]any); ok {
		if max, ok := budget["max_total_tokens"].(float64); ok {
			spec.Budget = int(max)
		}
	}
	return spec
}

// dispatchChild handles a child.request (spec §25.18-19): it scores the delegation against the
// parent's depth/fan-out/budget/capability, and on a deny journals child.denied.v1 and replies a
// denied child.result the engine folds (required → fail, optional → skip). On an admit it creates a
// ChildRun (parent_run_id, its own model/budget), runs it INLINE through the existing ExecuteAttempt
// path — a nested run with its own attempt, engine, and model call (its own chatcmpl), never a
// second queued job a single worker would deadlock on — then journals child.completed.v1 and replies
// the child's typed result (status + output + child run id). No secret ever reaches the child: it
// routes the same broker the parent does.
func (o *Orchestrator) dispatchChild(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	spec := decodeChildSpec(frame.Data)

	policy, err := o.spine.ProjectConfig(ctx, st.tenant)
	if err != nil {
		return err
	}
	parentTools := o.parentTools(ctx, st)
	remaining, bounded := st.budgetRemaining()
	admission := admitChild(spec, st.depth, len(st.childRunIDs), remaining, bounded, policy, parentTools)
	if admission.Denied {
		denied, _ := json.Marshal(map[string]any{"child_request_id": spec.ChildRequestID, "role": spec.Role, "model": spec.Model, "required": spec.Required, "reason": admission.Reason})
		if err := o.spine.JournalChildEvent(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), eventChildDenied, denied); err != nil {
			return err
		}
		return st.ch.Send(ctx, o.frame(st, "child.result", map[string]any{
			"child_request_id": spec.ChildRequestID, "status": "denied", "reason": admission.Reason,
		}, string(frame.ID)))
	}

	childRunID, childResponseID := newExecID("run"), newExecID("resp")
	childInput, _ := json.Marshal(spec.Objective)
	childDelegation, _ := json.Marshal(runDelegation{Spec: &delegationSpec{
		Role: spec.Role, Objective: spec.Objective, Model: spec.Model, Tools: spec.Tools,
		Budget: admission.EffectiveBudget, WorkspaceMode: spec.WorkspaceMode, Required: spec.Required,
	}})
	requested, _ := json.Marshal(map[string]any{"child_run_id": childRunID, "child_request_id": spec.ChildRequestID, "role": spec.Role, "model": spec.Model, "required": spec.Required})
	if err := o.spine.CreateChildRun(ctx, st.tenant, coordinator.ChildRunInput{
		ParentRunID: string(st.attempt.RunID), ParentResponseID: st.responseID, SessionID: st.sessionID,
		ChildRunID: childRunID, ChildResponseID: childResponseID, Depth: st.depth + 1,
		Input: childInput, Delegation: childDelegation, Store: true,
	}, eventChildRequested, requested); err != nil {
		return err
	}
	st.childRunIDs = append(st.childRunIDs, childRunID)
	st.childReserved += admission.EffectiveBudget

	// Run the child inline on the existing execution path. A child error (its own dial/loop
	// failure) is not the parent's — the child run row is authoritative, so we read its committed
	// outcome regardless and report it to the engine, which decides required-vs-optional.
	_ = o.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(childRunID), AttemptID: newAttemptID(), Fence: st.attempt.Fence,
		ImageDigest: st.attempt.ImageDigest, Limits: defaultAttemptLimits,
	})

	runState, output, err := o.spine.ChildRunOutcome(ctx, st.tenant, childRunID)
	if err != nil {
		return err
	}
	status := childStatus(runState)
	completed, _ := json.Marshal(map[string]any{"child_run_id": childRunID, "child_request_id": spec.ChildRequestID, "status": status})
	if err := o.spine.JournalChildEvent(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), eventChildCompleted, completed); err != nil {
		return err
	}
	return st.ch.Send(ctx, o.frame(st, "child.result", map[string]any{
		"child_request_id": spec.ChildRequestID, "status": status, "child_run_id": childRunID,
		"output": childOutputText(output),
	}, string(frame.ID)))
}

// parentTools is the parent's effective capability, which a child's tool subset must stay within
// (the parent half of the parent ∩ project intersection): the parent's session config tools, or
// unrestricted when it never narrowed them.
func (o *Orchestrator) parentTools(ctx context.Context, st *attemptState) []string {
	override, found, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil || !found {
		return nil
	}
	return override.Tools
}

// childStatus maps a ChildRun's terminal run state to the child.result status the engine folds: a
// completed child is a typed result, any other terminal (failed/canceled/timed_out/budget) is a
// non-completion the parent treats per the delegation's required flag.
func childStatus(runState string) string {
	if runState == "completed" {
		return "completed"
	}
	return "failed"
}

// childOutputText extracts the child's final text from its response projection so the parent folds a
// typed result, not a hidden transcript. A missing/again-shaped projection yields "".
func childOutputText(projection []byte) string {
	if len(projection) == 0 {
		return ""
	}
	var proj struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(projection, &proj); err != nil {
		return ""
	}
	for _, item := range proj.Output {
		if content, ok := item["content"].(string); ok && content != "" {
			return content
		}
	}
	return ""
}

// newExecID mints an envelope-valid id with the given prefix for a ChildRun's run and response
// rows, minted in the execution layer where the child is created (spec §25.18).
func newExecID(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "_" + hex.EncodeToString(raw[:])
}
