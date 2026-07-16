package statemachines

import (
	"errors"
	"testing"
)

func TestRunQueueDeadlineTimesOut(t *testing.T) {
	to, event, err := Apply(RunQueued, RunCmdTimeout, RunTable)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if to != RunTimedOut {
		t.Errorf("state: got %v, want %v", to, RunTimedOut)
	}
	if event != "run.timed_out.v1" {
		t.Errorf("event: got %q, want %q", event, "run.timed_out.v1")
	}
}

func TestRunTerminalityIsMonotonic(t *testing.T) {
	commands := []RunCommand{
		RunCmdProvision, RunCmdStart, RunCmdWait, RunCmdResume,
		RunCmdComplete, RunCmdFail, RunCmdCancel, RunCmdTimeout, RunCmdExhaustBudget,
	}
	for _, cmd := range commands {
		if _, _, err := Apply(RunCompleted, cmd, RunTable); !errors.Is(err, ErrInvalidState) {
			t.Errorf("Apply(completed, %v): got %v, want ErrInvalidState", cmd, err)
		}
	}
}
