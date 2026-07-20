package execution

import (
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// TestAdmitChildDepthAndFanoutBounded proves the deterministic depth/fan-out gate (SUB-004,
// spec §25.18): recursive delegation is off (a child at the depth cap is denied), and a parent
// that has already spawned the fan-out limit admits no more — no escaped child.
func TestAdmitChildDepthAndFanoutBounded(t *testing.T) {
	spec := childSpec{Model: "m", Required: true}
	var policy coordinator.ConfigPolicy // unrestricted

	// A root run (depth 0) admits its first child; a child (depth 1) may not delegate further.
	if got := admitChild(spec, 0, 0, 0, false, policy, nil); got.Denied {
		t.Fatalf("root delegation denied = %q, want admitted", got.Reason)
	}
	if got := admitChild(spec, maxChildDepth, 0, 0, false, policy, nil); !got.Denied || got.Reason != "depth_exceeded" {
		t.Fatalf("recursive delegation admission = %+v, want denied depth_exceeded", got)
	}
	// At the fan-out limit no further child is admitted.
	if got := admitChild(spec, 0, maxChildFanout, 0, false, policy, nil); !got.Denied || got.Reason != "fanout_exceeded" {
		t.Fatalf("over-fanout admission = %+v, want denied fanout_exceeded", got)
	}
	if got := admitChild(spec, 0, maxChildFanout-1, 0, false, policy, nil); got.Denied {
		t.Fatalf("under-fanout delegation denied = %q, want admitted", got.Reason)
	}
}

// TestAdmitChildEnforcesWorkspaceMode proves the E09 Task 6 enforcement of the T1 declare-only enum
// (spec §30.5): a valid workspace_mode (none/read_only/isolated, or empty = none) is admitted and
// resolves to the right writability; an unrecognized mode is denied rather than dispatched with an
// unknown workspace.
func TestAdmitChildEnforcesWorkspaceMode(t *testing.T) {
	var policy coordinator.ConfigPolicy // unrestricted

	for _, mode := range []string{"", "none", "read_only", "isolated"} {
		if got := admitChild(childSpec{Model: "m", WorkspaceMode: mode}, 0, 0, 0, false, policy, nil); got.Denied {
			t.Fatalf("workspace_mode %q admission = %+v, want admitted", mode, got)
		}
	}
	if got := admitChild(childSpec{Model: "m", WorkspaceMode: "wide-open"}, 0, 0, 0, false, policy, nil); !got.Denied || got.Reason != "invalid_workspace_mode" {
		t.Fatalf("invalid workspace_mode admission = %+v, want denied invalid_workspace_mode", got)
	}

	// Resolution: read_only is not writable, isolated is, none is not.
	for mode, wantWritable := range map[string]bool{"none": false, "read_only": false, "isolated": true} {
		plan, ok := resolveChildWorkspace(mode)
		if !ok || plan.Writable != wantWritable {
			t.Fatalf("resolveChildWorkspace(%q) = %+v ok=%v, want writable=%v", mode, plan, ok, wantWritable)
		}
	}
}

// TestAdmitChildBudgetIntersectsParentRemainder proves the budget half (SUB-004, spec §25.18):
// a child's requested budget is intersected with the parent's remainder — clamped when it
// over-requests, passed through under an unbounded parent, and denied when the parent is exhausted.
func TestAdmitChildBudgetIntersectsParentRemainder(t *testing.T) {
	var policy coordinator.ConfigPolicy

	// Bounded parent, 100 left: an over-request (150) clamps to 100; an under-request keeps 40.
	if got := admitChild(childSpec{Model: "m", Budget: 150}, 0, 0, 100, true, policy, nil); got.Denied || got.EffectiveBudget != 100 {
		t.Fatalf("over-request admission = %+v, want admitted budget 100 (clamped to remainder)", got)
	}
	if got := admitChild(childSpec{Model: "m", Budget: 40}, 0, 0, 100, true, policy, nil); got.Denied || got.EffectiveBudget != 40 {
		t.Fatalf("under-request admission = %+v, want admitted budget 40", got)
	}
	// A child requesting unbounded (0) under a bounded parent inherits exactly the remainder.
	if got := admitChild(childSpec{Model: "m", Budget: 0}, 0, 0, 100, true, policy, nil); got.Denied || got.EffectiveBudget != 100 {
		t.Fatalf("unbounded request under bounded parent = %+v, want admitted budget 100", got)
	}
	// An exhausted parent funds no child.
	if got := admitChild(childSpec{Model: "m", Budget: 10}, 0, 0, 0, true, policy, nil); !got.Denied || got.Reason != "budget_exhausted" {
		t.Fatalf("exhausted-parent admission = %+v, want denied budget_exhausted", got)
	}
	// An unbounded parent passes the request straight through.
	if got := admitChild(childSpec{Model: "m", Budget: 250}, 0, 0, 0, false, policy, nil); got.Denied || got.EffectiveBudget != 250 {
		t.Fatalf("unbounded-parent admission = %+v, want admitted budget 250", got)
	}
}

// TestAdmitChildCapabilityAndRoutability proves capability = parent ∩ project and routability
// (spec §25.18): a tool outside the intersection or a model outside the project allowlist is a
// deterministic deny — no silent broadening (SUB-003's unroutable half is the model case).
func TestAdmitChildCapabilityAndRoutability(t *testing.T) {
	// Project allows model "cheap-1" and tools {a,b}; parent narrows tools to {a}.
	policy := coordinator.ConfigPolicy{AllowedModels: []string{"cheap-1"}, AllowedTools: []string{"a", "b"}}
	parentTools := []string{"a"}

	if got := admitChild(childSpec{Model: "cheap-1", Tools: []string{"a"}}, 0, 0, 0, false, policy, parentTools); got.Denied {
		t.Fatalf("in-capability delegation denied = %q, want admitted", got.Reason)
	}
	// Tool "b" is allowed by the project but NOT by the parent — outside the intersection.
	if got := admitChild(childSpec{Model: "cheap-1", Tools: []string{"b"}}, 0, 0, 0, false, policy, parentTools); !got.Denied || got.Reason != "capability_denied" {
		t.Fatalf("parent-excluded tool admission = %+v, want denied capability_denied", got)
	}
	// A model outside the project allowlist is unroutable.
	if got := admitChild(childSpec{Model: "expensive-9", Tools: []string{"a"}}, 0, 0, 0, false, policy, parentTools); !got.Denied || got.Reason != "unroutable" {
		t.Fatalf("off-allowlist model admission = %+v, want denied unroutable", got)
	}
}
