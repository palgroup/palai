package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
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
// never be an unknown mode. The isolated/read_only worktree is REALIZED by realizeChildWorkspace
// (below) via the repositories worktree mechanics (AddIsolatedWorktree / AddReadOnlyWorktree) when the
// parent holds a workspace — a child never takes a second writer lease (spec §29.8), it branches the
// parent's checkout copy-on-write and merges back only explicitly (E09 Task 10 closes the T6 debt).
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

// realizeChildWorkspace materializes a delegated child's workspace off the PARENT's checkout (spec
// §30.5): a copy-on-write git worktree at <parent-allocation>/children/<child>/repo, on the child's own
// branch (isolated, writable) or detached + write-denied (read_only). It shares the parent's git object
// store, so there is NO clone and NO second writer lease — the child's edits reach the parent only via an
// explicit merge (REP-011). It returns the child's allocation root (its tools' WorkspaceRoot; the
// worktree is its repo dir).
// ponytail: the worktree is left in place under the parent allocation until the allocation is destroyed
// (E10) — cheap (shared objects) and each child dir is unique (child run id), so nothing collides.
// ponytail ceiling: git worktree writes a .git FILE pointing at the parent repo's HOST-ABSOLUTE path
// (gitdir: <parent>/.git/worktrees/<name>), so raw `git` inside a child SANDBOX with a different mount
// path cannot resolve it — a split CP≠runner sandbox needs the worktree rebased to the mount path. The
// CP-side commit tool operates on the host path directly, so it is unaffected here (collapsed compose).
func (o *Orchestrator) realizeChildWorkspace(ctx context.Context, st *attemptState, childRunID string, ws childWorkspace) (string, error) {
	parentRepo := filepath.Join(st.attempt.WorkspaceHostPath, workspace.RepoDir)
	base, _, err := repositories.Head(ctx, parentRepo)
	if err != nil {
		return "", err
	}
	childRoot := filepath.Join(st.attempt.WorkspaceHostPath, "children", childRunID)
	if err := os.MkdirAll(childRoot, 0o755); err != nil {
		return "", err
	}
	worktreePath := filepath.Join(childRoot, workspace.RepoDir)
	if ws.Writable {
		_, err = repositories.AddIsolatedWorktree(ctx, parentRepo, worktreePath, st.sessionID, childRunID, base)
	} else {
		_, err = repositories.AddReadOnlyWorktree(ctx, parentRepo, worktreePath, base)
	}
	if err != nil {
		return "", err
	}
	return childRoot, nil
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
	// Detach requests the child run as a DETACHED durable job (spec §25.18-19, E10 T8 DET-001): the
	// parent releases its compute at this boundary and the child terminal wakes it, rather than the
	// parent holding the engine idle while the child runs inline. It rides the child.request frame.
	Detach bool
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
// if every requested tool is within it. A NIL parentTools is unrestricted (no ceiling and no session
// override); a NON-nil set — even EMPTY — is an explicit ceiling, so an empty one is "select no tools"
// (spec §14.2) and denies every child tool. Testing nil (not len>0) closes the delegation-ceiling
// escape where an allow-nothing ceiling, or a ceiling∩override that emptied out, read as unrestricted.
func capabilityDeniedTool(childTools, parentTools []string, policy coordinator.ConfigPolicy) string {
	if parentTools != nil {
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
	// Detach (on a root run's emit spec) requests the child run detached (E10 T8, DET-001). It rides
	// the run.start delegations verbatim, so a config-seeded delegation can detach.
	Detach bool `json:"detach,omitempty"`
	// ChildRequestID + Detached ride a CHILD run's own spec (not an emit spec): the linkage a restored
	// parent's re-emitted child.request rebinds by, so a detached parent-resume never clones the child
	// (E10 T8 keystone). Stored in the child's delegation.spec JSONB — no separate column, no migration.
	ChildRequestID string `json:"child_request_id,omitempty"`
	Detached       bool   `json:"detached,omitempty"`
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
		frame := map[string]any{
			"role": s.Role, "objective": s.Objective, "model": s.Model,
			"tools": s.Tools, "budget": map[string]any{"max_total_tokens": s.Budget},
			"workspace_mode": mode, "required": s.Required,
		}
		// Carry the detach request only when set, so a non-detaching delegation's child.request stays
		// byte-identical to before (E10 T8) — the request_hash and existing golden frames are unchanged.
		if s.Detach {
			frame["detach"] = true
		}
		out = append(out, frame)
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
	spec.Detach, _ = data["detach"].(bool)
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

// dispatchChild handles a child.request (spec §25.18-19). It is the single home of two paths (E10 T8
// fork 5): a FRESH request is scored against the parent's depth/fan-out/budget/capability and either
// denied (child.denied.v1 + a denied child.result the engine folds) or admitted into a ChildRun; a
// RE-EMITTED request — a restored parent re-emits the SAME deterministic child.request — is REBOUND to
// the existing child rather than cloned (the DET-001 keystone). An admitted child runs INLINE by
// default (a nested ExecuteAttempt on this goroutine) OR, when it asks to detach (E10 T8), as a durable
// job while the parent releases its compute. No secret ever reaches the child: it routes the same
// broker the parent does.
func (o *Orchestrator) dispatchChild(ctx context.Context, st *attemptState, frame contracts.EngineFrame) error {
	spec := decodeChildSpec(frame.Data)

	// An empty child_request_id is a malformed frame (the engine always sets the deterministic id): it
	// would match nothing in the JSONB rebind lookup, so every restore would silently RE-CLONE the child
	// (m-6). Fail the attempt loudly rather than let the keystone rot.
	if spec.ChildRequestID == "" {
		return fmt.Errorf("child.request carries no child_request_id: the re-emit rebind key would match nothing")
	}

	// Re-emit rebind (the keystone): a parent restore re-emits its pending child.request with the SAME
	// deterministic child_request_id. If a child already exists for it, bind that one — never a second
	// ChildRun — folding its terminal result now, or re-releasing while it still runs.
	if existing, found, err := o.spine.LookupChildByRequest(ctx, st.tenant, string(st.attempt.RunID), spec.ChildRequestID); err != nil {
		return err
	} else if found {
		return o.rebindChild(ctx, st, spec, existing, frame)
	}

	policy, err := o.spine.ProjectConfig(ctx, st.tenant)
	if err != nil {
		return err
	}
	parentTools, err := o.parentTools(ctx, st)
	if err != nil {
		return err
	}
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

	childWS, _ := resolveChildWorkspace(spec.WorkspaceMode) // admitChild already validated the mode
	// Detach is honored only for a workspace-less child and only when a checkpoint sink is wired (§26.5:
	// no durable boundary ⇒ no release). A detach request that cannot be honored falls through to the
	// inline path — the safe default. ponytail: isolated/read_only + detach (the child's worktree lives
	// under the parent allocation the release would tear down) is a named ceiling, inline for now.
	detach := spec.Detach && childWS.Mode == workspaceModeNone && o.checkpoints != nil

	childRunID, childResponseID := newExecID("run"), newExecID("resp")
	childInput, _ := json.Marshal(spec.Objective)
	childDelegation, _ := json.Marshal(runDelegation{Spec: &delegationSpec{
		Role: spec.Role, Objective: spec.Objective, Model: spec.Model, Tools: spec.Tools,
		Budget: admission.EffectiveBudget, WorkspaceMode: spec.WorkspaceMode, Required: spec.Required,
		// The child carries its own linkage so a restored parent rebinds it (E10 T8): the request id it
		// answers and whether it was detached, both on its delegation.spec JSONB (no separate column).
		ChildRequestID: spec.ChildRequestID, Detached: detach,
	}})
	requested, _ := json.Marshal(map[string]any{"child_run_id": childRunID, "child_request_id": spec.ChildRequestID, "role": spec.Role, "model": spec.Model, "required": spec.Required})
	if err := o.spine.CreateChildRun(ctx, st.tenant, coordinator.ChildRunInput{
		ParentRunID: string(st.attempt.RunID), ParentResponseID: st.responseID, SessionID: st.sessionID,
		ChildRunID: childRunID, ChildResponseID: childResponseID, Depth: st.depth + 1,
		Input: childInput, Delegation: childDelegation, Store: true,
		// A detached child's response.run job commits ATOMICALLY with its row (MF-3): no jobless orphan.
		EnqueueRun: detach,
	}, eventChildRequested, requested); err != nil {
		return err
	}
	st.childRunIDs = append(st.childRunIDs, childRunID)
	st.childReserved += admission.EffectiveBudget

	// Detached path (E10 T8, DET-001): the child's durable job is already enqueued (atomically, above), so
	// RELEASE the parent — checkpoint this awaiting-children boundary, drive the run to waiting, and end the
	// attempt. The child terminal wakes the parent (finalize → WakeParentOfChild), which restores + rebinds.
	if detach {
		return o.releaseParentForDetach(ctx, st)
	}

	// Realize the child's workspace when it asked for one and the parent holds one (spec §30.5, §29.8):
	// a copy-on-write worktree off the parent's checkout — isolated (writable, own branch) or read_only
	// (write-denied snapshot) — set on the child's descriptor so its file/shell tools confine to it. The
	// child never takes a writer lease: ExecuteAttempt's root-only (depth 0) provisioning gate skips a
	// depth-1 child, and the worktree shares the parent's object store without a second single-writer slot.
	childDesc := AttemptDescriptor{
		RunID: contracts.RunID(childRunID), AttemptID: newAttemptID(), Fence: st.attempt.Fence,
		ImageDigest: st.attempt.ImageDigest, Limits: defaultAttemptLimits,
	}
	if childWS.Mode != workspaceModeNone && st.attempt.WorkspaceHostPath != "" {
		hostPath, err := o.realizeChildWorkspace(ctx, st, childRunID, childWS)
		if err != nil {
			return err
		}
		childDesc.WorkspaceHostPath, childDesc.WorkspaceReadOnly = hostPath, !childWS.Writable
	}

	// Run the child inline on the existing execution path. A child error (its own dial/loop
	// failure) is not the parent's — the child run row is authoritative, so we read its committed
	// outcome regardless and report it to the engine, which decides required-vs-optional.
	_ = o.ExecuteAttempt(ctx, childDesc)
	return o.foldChildResult(ctx, st, spec.ChildRequestID, childRunID, frame)
}

// rebindChild handles a re-emitted child.request whose child already exists (E10 T8, DET-001 keystone):
// a terminal child folds its typed result now (never a re-spawn); a still-running DETACHED child re-
// releases the parent (a later child terminal re-wakes it); a still-running INLINE child (its previous
// inline run crashed with the parent, so it has no durable job and no waker — MF-2) is RE-EXECUTED inline
// now, never released into a permanent hang. It appends the child to this attempt's linkage set (so the
// restored parent's terminal projection still links it) and re-reserves its budget against the parent
// (m-4: sequential detach cycles must not over-fund children past the SUB-004 bound).
func (o *Orchestrator) rebindChild(ctx context.Context, st *attemptState, spec childSpec, existing coordinator.ChildRunLookup, frame contracts.EngineFrame) error {
	if !slices.Contains(st.childRunIDs, existing.RunID) {
		st.childRunIDs = append(st.childRunIDs, existing.RunID)
		st.childReserved += existing.Budget
	}
	if childRunTerminal(existing.State) {
		return o.foldChildResult(ctx, st, spec.ChildRequestID, existing.RunID, frame)
	}
	if existing.Detached {
		// The detached child still runs as a durable job: release the parent again and wait for its
		// terminal wake (fan-out sibling still working, or a child→parent event woke the parent early).
		return o.releaseParentForDetach(ctx, st)
	}
	// A non-terminal INLINE child crashed with the parent's previous attempt — no durable job, no waker
	// (MF-2). Re-execute it inline on the existing execution path (rung-1 stand-down guards a genuinely
	// live sibling), then fold — the parent makes progress instead of releasing into a hang.
	// ponytail: a re-executed inline child re-runs without re-realizing a worktree; a crashed
	// WORKSPACE-mode inline child is a named gap (rare — depth-1, worktree persists under the parent).
	childDesc := AttemptDescriptor{
		RunID: contracts.RunID(existing.RunID), AttemptID: newAttemptID(), Fence: st.attempt.Fence,
		ImageDigest: st.attempt.ImageDigest, Limits: defaultAttemptLimits,
	}
	_ = o.ExecuteAttempt(ctx, childDesc)
	return o.foldChildResult(ctx, st, spec.ChildRequestID, existing.RunID, frame)
}

// foldChildResult reads a finished child's committed outcome, journals child.completed.v1 on the
// parent (exactly-once across restores), and replies the typed child.result the engine folds (spec
// §25.19). Shared by the inline dispatch and the detached rebind so the fold is one code path.
func (o *Orchestrator) foldChildResult(ctx context.Context, st *attemptState, childRequestID, childRunID string, frame contracts.EngineFrame) error {
	runState, output, err := o.spine.ChildRunOutcome(ctx, st.tenant, childRunID)
	if err != nil {
		return err
	}
	status := childStatus(runState)
	completed, _ := json.Marshal(map[string]any{"child_run_id": childRunID, "child_request_id": childRequestID, "status": status})
	if err := o.spine.JournalChildCompletionOnce(ctx, st.tenant, st.sessionID, st.responseID, string(st.attempt.RunID), eventChildCompleted, childRunID, completed); err != nil {
		return err
	}
	return st.ch.Send(ctx, o.frame(st, "child.result", map[string]any{
		"child_request_id": childRequestID, "status": status, "child_run_id": childRunID,
		"output": childOutputText(output),
	}, string(frame.ID)))
}

// childRunTerminal reports whether a run state is terminal, so a rebind folds a finished child rather
// than waiting on it (E10 T8).
func childRunTerminal(state string) bool {
	switch state {
	case "completed", "failed", "canceled", "timed_out", "budget_exceeded":
		return true
	default:
		return false
	}
}

// parentTools is the parent's effective capability, which a child's tool subset must stay within
// (the parent half of the parent ∩ project intersection): the parent's session config tools capped by
// its pinned-revision ceiling. The revision ceiling is load-bearing here — WITHOUT it a run pinned to
// tools=[file] whose session never overrode tools would expose nil (=unrestricted), letting a child be
// granted any project-baseline tool and EXPANDING capability through delegation (spec §10, 63.4). It
// reads through the SAME PinnedExecConfig the resolver uses, and fails CLOSED on a DB error (returns it,
// so the delegation is refused rather than silently unrestricted).
func (o *Orchestrator) parentTools(ctx context.Context, st *attemptState) ([]string, error) {
	override, found, err := o.spine.LatestSessionConfig(ctx, st.tenant, st.sessionID)
	if err != nil {
		return nil, err
	}
	tools := []string(nil)
	if found {
		tools = override.Tools
	}
	_, _, revTools, _, _, err := o.spine.PinnedExecConfig(ctx, st.tenant, string(st.attempt.RunID))
	if err != nil {
		return nil, err
	}
	if revTools != nil {
		if tools == nil {
			tools = revTools
		} else {
			tools = intersectTools(tools, revTools)
		}
	}
	return tools, nil
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
