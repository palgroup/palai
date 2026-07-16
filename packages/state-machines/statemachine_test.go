package statemachines

import (
	"errors"
	"testing"
)

func TestApplyReturnsTableRow(t *testing.T) {
	to, event, err := Apply(RunQueued, RunCmdProvision, RunTable)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if to != RunProvisioning {
		t.Errorf("state: got %v, want %v", to, RunProvisioning)
	}
	if event != "run.provisioning.v1" {
		t.Errorf("event: got %q, want %q", event, "run.provisioning.v1")
	}
}

func TestApplyRejectsUnknownTransitionWithInvalidState(t *testing.T) {
	to, event, err := Apply(RunCompleted, RunCmdProvision, RunTable)
	if !errors.Is(err, ErrInvalidState) {
		t.Fatalf("error: got %v, want ErrInvalidState", err)
	}
	var zero RunState
	if to != zero {
		t.Errorf("state: got %v, want zero value", to)
	}
	if event != "" {
		t.Errorf("event: got %q, want empty", event)
	}
}

func TestTerminalStatesHaveNoOutgoingRows(t *testing.T) {
	assertTerminals(t, RunTable, map[RunState]bool{
		RunCompleted:      true,
		RunFailed:         true,
		RunCanceled:       true,
		RunTimedOut:       true,
		RunBudgetExceeded: true,
	})
	assertTerminals(t, AttemptTable, map[AttemptState]bool{
		AttemptSucceeded: true,
		AttemptFailed:    true,
		AttemptLost:      true,
		AttemptPreempted: true,
	})
}

// assertTerminals checks that TerminalStates marks exactly wantTerminal and that
// no terminal state is the source of a transition.
func assertTerminals[S comparable, C comparable](t *testing.T, table []Transition[S, C], wantTerminal map[S]bool) {
	t.Helper()
	terminal := TerminalStates(table)
	for state, isTerminal := range terminal {
		if isTerminal != wantTerminal[state] {
			t.Errorf("state %v: terminal=%v, want %v", state, isTerminal, wantTerminal[state])
		}
	}
	for _, tr := range table {
		if terminal[tr.From] {
			t.Errorf("terminal state %v has outgoing command %v", tr.From, tr.Command)
		}
	}
}

func TestAttemptRequiresIncreasingFence(t *testing.T) {
	if err := AcceptFence(5, 5); !errors.Is(err, ErrStaleFence) {
		t.Errorf("AcceptFence(5,5): got %v, want ErrStaleFence", err)
	}
	if err := AcceptFence(5, 4); !errors.Is(err, ErrStaleFence) {
		t.Errorf("AcceptFence(5,4): got %v, want ErrStaleFence", err)
	}
	if err := AcceptFence(5, 6); err != nil {
		t.Errorf("AcceptFence(5,6): got %v, want nil", err)
	}
}

func TestNextSequenceIsStrictlyMonotonic(t *testing.T) {
	if err := NextSequence(7, 8); err != nil {
		t.Errorf("NextSequence(7,8): got %v, want nil", err)
	}
	if err := NextSequence(7, 7); !errors.Is(err, ErrNonMonotonicSequence) {
		t.Errorf("NextSequence(7,7): got %v, want ErrNonMonotonicSequence", err)
	}
	if err := NextSequence(7, 6); !errors.Is(err, ErrNonMonotonicSequence) {
		t.Errorf("NextSequence(7,6): got %v, want ErrNonMonotonicSequence", err)
	}
}
