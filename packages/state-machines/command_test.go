package statemachines

import (
	"errors"
	"testing"
)

func TestCommandDuplicateApplicationIsInvalid(t *testing.T) {
	// A command that already reached applied cannot be applied again (spec §22.4:
	// duplicate command IDs return the original result, they never re-apply).
	if _, _, err := Apply(CommandApplied, CommandCmdApply, CommandTable); !errors.Is(err, ErrInvalidState) {
		t.Errorf("Apply(applied, apply): got %v, want ErrInvalidState", err)
	}
	// The one legal path into applied is queued → applying → applied.
	to, _, err := Apply(CommandQueued, CommandCmdApply, CommandTable)
	if err != nil || to != CommandApplying {
		t.Fatalf("Apply(queued, apply): got (%v, %v), want (applying, nil)", to, err)
	}
	to, _, err = Apply(to, CommandCmdFinishApply, CommandTable)
	if err != nil || to != CommandApplied {
		t.Fatalf("Apply(applying, finish_apply): got (%v, %v), want (applied, nil)", to, err)
	}
}
