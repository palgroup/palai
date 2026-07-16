package statemachines

import (
	"errors"
	"reflect"
	"testing"
)

func TestToolCallUncertainCannotBecomeCompletedWithoutReconciliation(t *testing.T) {
	// From uncertain, complete is invalid; only reconcile_completed,
	// reconcile_not_applied, and escalate move it forward (spec §26.7).
	if _, _, err := Apply(ToolCallUncertain, ToolCallCmdComplete, ToolCallTable); !errors.Is(err, ErrInvalidState) {
		t.Errorf("Apply(uncertain, complete): got %v, want ErrInvalidState", err)
	}
	valid := map[ToolCallCommand]ToolCallState{
		ToolCallCmdReconcileCompleted:  ToolCallReconciledCompleted,
		ToolCallCmdReconcileNotApplied: ToolCallReconciledNotApplied,
		ToolCallCmdEscalate:            ToolCallManualResolution,
	}
	for cmd, want := range valid {
		to, _, err := Apply(ToolCallUncertain, cmd, ToolCallTable)
		if err != nil {
			t.Errorf("Apply(uncertain, %v): unexpected error: %v", cmd, err)
		}
		if to != want {
			t.Errorf("Apply(uncertain, %v): got %v, want %v", cmd, to, want)
		}
	}
}

func TestToolCallOnlyCompletedFamiliesAreSuccessful(t *testing.T) {
	// spec §26.7: only completed and reconciled_completed enter context as
	// successful tool results.
	want := map[ToolCallState]bool{
		ToolCallCompleted:           true,
		ToolCallReconciledCompleted: true,
	}
	if got := SuccessfulToolStates(); !reflect.DeepEqual(got, want) {
		t.Errorf("SuccessfulToolStates() = %v, want %v", got, want)
	}
}
