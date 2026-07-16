package statemachines

import (
	"errors"
	"testing"
)

func TestWorkspaceLeaseCyclesThroughReadyAndSnapshotting(t *testing.T) {
	// ready↔leased and ready↔snapshotting both return to ready (spec §29.7).
	steps := []struct {
		from    WorkspaceState
		command WorkspaceCommand
		to      WorkspaceState
		event   string
	}{
		{WorkspaceReady, WorkspaceCmdLease, WorkspaceLeased, "workspace.leased.v1"},
		{WorkspaceLeased, WorkspaceCmdRelease, WorkspaceReady, "workspace.ready.v1"},
		{WorkspaceReady, WorkspaceCmdSnapshot, WorkspaceSnapshotting, "workspace.snapshotting.v1"},
		{WorkspaceSnapshotting, WorkspaceCmdFinishSnapshot, WorkspaceReady, "workspace.ready.v1"},
	}
	for _, s := range steps {
		to, event, err := Apply(s.from, s.command, WorkspaceTable)
		if err != nil {
			t.Fatalf("Apply(%v, %v): unexpected error: %v", s.from, s.command, err)
		}
		if to != s.to {
			t.Errorf("Apply(%v, %v): got %v, want %v", s.from, s.command, to, s.to)
		}
		if event != s.event {
			t.Errorf("Apply(%v, %v): event %q, want %q", s.from, s.command, event, s.event)
		}
	}
}

func TestWorkspaceDestroyAllowedOnlyFromReadyPausedFailed(t *testing.T) {
	// destroy is valid only from ready, paused, and failed (spec §29.7).
	allowed := map[WorkspaceState]bool{
		WorkspaceReady:  true,
		WorkspacePaused: true,
		WorkspaceFailed: true,
	}
	all := []WorkspaceState{
		WorkspaceRequested, WorkspaceProvisioning, WorkspacePreparing, WorkspaceReady,
		WorkspaceLeased, WorkspaceSnapshotting, WorkspacePaused, WorkspaceRestoring,
		WorkspaceHostLost, WorkspaceRecovering, WorkspaceFailed, WorkspaceDestroying,
		WorkspaceDestroyed,
	}
	for _, from := range all {
		to, event, err := Apply(from, WorkspaceCmdDestroy, WorkspaceTable)
		if allowed[from] {
			if err != nil {
				t.Errorf("Apply(%v, destroy): unexpected error: %v", from, err)
			}
			if to != WorkspaceDestroying {
				t.Errorf("Apply(%v, destroy): got %v, want %v", from, to, WorkspaceDestroying)
			}
			if event != "workspace.destroying.v1" {
				t.Errorf("Apply(%v, destroy): event %q, want %q", from, event, "workspace.destroying.v1")
			}
			continue
		}
		if !errors.Is(err, ErrInvalidState) {
			t.Errorf("Apply(%v, destroy): got %v, want ErrInvalidState", from, err)
		}
	}
}
