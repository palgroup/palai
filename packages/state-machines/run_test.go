package statemachines

import (
	"errors"
	"testing"
)

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
