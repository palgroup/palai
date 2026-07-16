package statemachines

// WorkspaceState is a Workspace lifecycle state (spec §29.7).
type WorkspaceState string

// WorkspaceCommand drives a Workspace state transition (spec §29.7).
type WorkspaceCommand string

const (
	WorkspaceRequested    WorkspaceState = "requested"
	WorkspaceProvisioning WorkspaceState = "provisioning"
	WorkspacePreparing    WorkspaceState = "preparing"
	WorkspaceReady        WorkspaceState = "ready"
	WorkspaceLeased       WorkspaceState = "leased"
	WorkspaceSnapshotting WorkspaceState = "snapshotting"
	WorkspacePaused       WorkspaceState = "paused"
	WorkspaceRestoring    WorkspaceState = "restoring"
	WorkspaceHostLost     WorkspaceState = "host_lost"
	WorkspaceRecovering   WorkspaceState = "recovering"
	WorkspaceFailed       WorkspaceState = "failed"
	WorkspaceDestroying   WorkspaceState = "destroying"
	WorkspaceDestroyed    WorkspaceState = "destroyed"
)

const (
	WorkspaceCmdProvision      WorkspaceCommand = "provision"
	WorkspaceCmdPrepare        WorkspaceCommand = "prepare"
	WorkspaceCmdMarkReady      WorkspaceCommand = "mark_ready"
	WorkspaceCmdLease          WorkspaceCommand = "lease"
	WorkspaceCmdRelease        WorkspaceCommand = "release"
	WorkspaceCmdSnapshot       WorkspaceCommand = "snapshot"
	WorkspaceCmdFinishSnapshot WorkspaceCommand = "finish_snapshot"
	WorkspaceCmdPause          WorkspaceCommand = "pause"
	WorkspaceCmdRestore        WorkspaceCommand = "restore"
	WorkspaceCmdLoseHost       WorkspaceCommand = "lose_host"
	WorkspaceCmdRecover        WorkspaceCommand = "recover"
	WorkspaceCmdFail           WorkspaceCommand = "fail"
	WorkspaceCmdDestroy        WorkspaceCommand = "destroy"
	WorkspaceCmdFinishDestroy  WorkspaceCommand = "finish_destroy"
)

// WorkspaceTable is the Workspace transition table (spec §29.7). ready cycles
// with leased (lease/release) and with snapshotting (snapshot/finish_snapshot).
// preparing and ready pause, then restore through restoring back to ready. A
// leased workspace that loses its host recovers back to ready or fails. ready,
// paused, and failed destroy. mark_ready declares readiness from preparing,
// restoring, and recovering.
var WorkspaceTable = []Transition[WorkspaceState, WorkspaceCommand]{
	{WorkspaceRequested, WorkspaceCmdProvision, WorkspaceProvisioning, "workspace.provisioning.v1"},
	{WorkspaceProvisioning, WorkspaceCmdPrepare, WorkspacePreparing, "workspace.preparing.v1"},
	{WorkspacePreparing, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},

	{WorkspaceReady, WorkspaceCmdLease, WorkspaceLeased, "workspace.leased.v1"},
	{WorkspaceLeased, WorkspaceCmdRelease, WorkspaceReady, "workspace.ready.v1"},

	{WorkspaceReady, WorkspaceCmdSnapshot, WorkspaceSnapshotting, "workspace.snapshotting.v1"},
	{WorkspaceSnapshotting, WorkspaceCmdFinishSnapshot, WorkspaceReady, "workspace.ready.v1"},

	{WorkspacePreparing, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
	{WorkspaceReady, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
	{WorkspacePaused, WorkspaceCmdRestore, WorkspaceRestoring, "workspace.restoring.v1"},
	{WorkspaceRestoring, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},

	{WorkspaceLeased, WorkspaceCmdLoseHost, WorkspaceHostLost, "workspace.host_lost.v1"},
	{WorkspaceHostLost, WorkspaceCmdRecover, WorkspaceRecovering, "workspace.recovering.v1"},
	{WorkspaceRecovering, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
	{WorkspaceRecovering, WorkspaceCmdFail, WorkspaceFailed, "workspace.failed.v1"},

	{WorkspaceReady, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspacePaused, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspaceFailed, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspaceDestroying, WorkspaceCmdFinishDestroy, WorkspaceDestroyed, "workspace.destroyed.v1"},
}
