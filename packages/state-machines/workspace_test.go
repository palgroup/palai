package statemachines

import (
	"errors"
	"testing"
)

// TestWorkspaceBindingTransitions walks the WorkspaceBinding lifecycle spine and every side
// branch (spec §29.7), asserting each command maps to the exact destination and event, and that a
// few illegal jumps are rejected. The registry-sync (TestEveryTableEventExistsInRegistry) and the
// random-walk property (TestTerminalMonotonicityUnderRandomCommandSequences) already exercise the
// table through allTables; this pins the named transitions the same way the run/response SM tests do.
func TestWorkspaceBindingTransitions(t *testing.T) {
	legal := []struct {
		from    WorkspaceState
		command WorkspaceCommand
		to      WorkspaceState
		event   string
	}{
		// Provisioning spine: requested → provisioning → preparing → ready → leased → ready.
		{WorkspaceRequested, WorkspaceCmdProvision, WorkspaceProvisioning, "workspace.provisioning.v1"},
		{WorkspaceProvisioning, WorkspaceCmdPrepare, WorkspacePreparing, "workspace.preparing.v1"},
		{WorkspacePreparing, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
		{WorkspaceReady, WorkspaceCmdLease, WorkspaceLeased, "workspace.leased.v1"},
		{WorkspaceLeased, WorkspaceCmdRelease, WorkspaceReady, "workspace.ready.v1"},
		// Snapshot cycle off ready.
		{WorkspaceReady, WorkspaceCmdSnapshot, WorkspaceSnapshotting, "workspace.snapshotting.v1"},
		{WorkspaceSnapshotting, WorkspaceCmdFinishSnapshot, WorkspaceReady, "workspace.ready.v1"},
		// Pause/restore branch from preparing and ready.
		{WorkspacePreparing, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
		{WorkspaceReady, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
		{WorkspacePaused, WorkspaceCmdRestore, WorkspaceRestoring, "workspace.restoring.v1"},
		{WorkspaceRestoring, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
		// Host loss → recovery → ready or failed.
		{WorkspaceLeased, WorkspaceCmdLoseHost, WorkspaceHostLost, "workspace.host_lost.v1"},
		{WorkspaceHostLost, WorkspaceCmdRecover, WorkspaceRecovering, "workspace.recovering.v1"},
		{WorkspaceRecovering, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
		{WorkspaceRecovering, WorkspaceCmdFail, WorkspaceFailed, "workspace.failed.v1"},
		// Destruction from ready/paused/failed → destroyed.
		{WorkspaceReady, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
		{WorkspaceDestroying, WorkspaceCmdFinishDestroy, WorkspaceDestroyed, "workspace.destroyed.v1"},
	}
	for _, s := range legal {
		to, event, err := Apply(s.from, s.command, WorkspaceTable)
		if err != nil {
			t.Fatalf("Apply(%v, %v): unexpected error: %v", s.from, s.command, err)
		}
		if to != s.to || event != s.event {
			t.Errorf("Apply(%v, %v) = (%v, %q), want (%v, %q)", s.from, s.command, to, event, s.to, s.event)
		}
	}

	// A logical workspace cannot lease before it is ready, cannot snapshot while leased, and a
	// destroyed workspace accepts nothing further.
	illegal := []struct {
		from    WorkspaceState
		command WorkspaceCommand
	}{
		{WorkspaceRequested, WorkspaceCmdLease},
		{WorkspaceLeased, WorkspaceCmdSnapshot},
		{WorkspaceDestroyed, WorkspaceCmdProvision},
	}
	for _, s := range illegal {
		if _, _, err := Apply(s.from, s.command, WorkspaceTable); !errors.Is(err, ErrInvalidState) {
			t.Errorf("Apply(%v, %v): got %v, want ErrInvalidState", s.from, s.command, err)
		}
	}
}

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
