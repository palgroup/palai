package statemachines

import (
	"errors"
	"strings"
	"testing"
)

func TestSessionTerminalRunDoesNotCloseSession(t *testing.T) {
	sessionCommands := map[SessionCommand]bool{
		SessionCmdPause:       true,
		SessionCmdResume:      true,
		SessionCmdClose:       true,
		SessionCmdFinishClose: true,
		SessionCmdDelete:      true,
	}
	for _, tr := range SessionTable {
		if !sessionCommands[tr.Command] {
			t.Errorf("SessionTable has non-session command %q (from %v)", tr.Command, tr.From)
		}
		if !strings.HasPrefix(tr.Event, "session.") {
			t.Errorf("SessionTable row from %v emits non-session event %q", tr.From, tr.Event)
		}
	}
	// A terminal run must not jump a live session straight to closed or deleted;
	// only an explicit close moves active/paused forward (spec §22.1).
	for _, from := range []SessionState{SessionActive, SessionPaused} {
		for _, cmd := range []SessionCommand{SessionCmdFinishClose, SessionCmdDelete} {
			if _, _, err := Apply(from, cmd, SessionTable); !errors.Is(err, ErrInvalidState) {
				t.Errorf("Apply(%v, %v): got %v, want ErrInvalidState", from, cmd, err)
			}
		}
	}
}

func TestSessionDeleteRequiresClosed(t *testing.T) {
	to, event, err := Apply(SessionClosed, SessionCmdDelete, SessionTable)
	if err != nil {
		t.Fatalf("Apply(closed, delete): unexpected error: %v", err)
	}
	if to != SessionDeleted {
		t.Errorf("Apply(closed, delete): got %v, want %v", to, SessionDeleted)
	}
	if event != "session.deleted.v1" {
		t.Errorf("Apply(closed, delete): event %q, want %q", event, "session.deleted.v1")
	}
	for _, from := range []SessionState{SessionActive, SessionPaused, SessionClosing} {
		if _, _, err := Apply(from, SessionCmdDelete, SessionTable); !errors.Is(err, ErrInvalidState) {
			t.Errorf("Apply(%v, delete): got %v, want ErrInvalidState", from, err)
		}
	}
}
