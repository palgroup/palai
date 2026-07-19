package execution

import (
	"slices"

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
