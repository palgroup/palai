package statemachines

import (
	"errors"
	"testing"
)

func TestResponseWaitingStatesReturnOnlyToInProgress(t *testing.T) {
	waiting := []ResponseState{
		ResponseWaitingForTool,
		ResponseWaitingForApproval,
		ResponseWaitingForInput,
	}
	for _, from := range waiting {
		to, event, err := Apply(from, ResponseCmdResume, ResponseTable)
		if err != nil {
			t.Fatalf("Apply(%v, resume): unexpected error: %v", from, err)
		}
		if to != ResponseInProgress {
			t.Errorf("Apply(%v, resume): got %v, want %v", from, to, ResponseInProgress)
		}
		if event != "response.in_progress.v1" {
			t.Errorf("Apply(%v, resume): event %q, want %q", from, event, "response.in_progress.v1")
		}
	}
}

func TestResponseTerminalsAreMonotonic(t *testing.T) {
	commands := []ResponseCommand{
		ResponseCmdProvision, ResponseCmdStart, ResponseCmdRequestTool,
		ResponseCmdRequestApproval, ResponseCmdRequestInput, ResponseCmdResume,
		ResponseCmdComplete, ResponseCmdFail, ResponseCmdCancel,
		ResponseCmdTimeout, ResponseCmdExhaustBudget,
	}
	terminals := []ResponseState{
		ResponseCompleted, ResponseFailed, ResponseCanceled,
		ResponseTimedOut, ResponseBudgetExceeded,
	}
	for _, state := range terminals {
		for _, cmd := range commands {
			if _, _, err := Apply(state, cmd, ResponseTable); !errors.Is(err, ErrInvalidState) {
				t.Errorf("Apply(%v, %v): got %v, want ErrInvalidState", state, cmd, err)
			}
		}
	}
}
