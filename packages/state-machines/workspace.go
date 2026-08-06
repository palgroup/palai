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
// with leased (lease/release) and with snapshotting (snapshot/finish_snapshot),
// and snapshotting also PAUSES — the idle releaser's commit, which is why the
// cycle has an exit as well as a return.
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
	// snapshotting -> paused is the idle releaser's commit point, and it exists so the release never
	// passes back through `ready`. The releaser claims a workspace into snapshotting (which nothing can
	// lease from), archives it, and only then hands the machine back; without this edge it would have to
	// finish_snapshot to ready and pause from there, and `ready` is leasable — a run acquiring the
	// workspace in that window would be handed a directory the releaser is about to delete.
	{WorkspaceSnapshotting, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},

	{WorkspacePreparing, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
	{WorkspaceReady, WorkspaceCmdPause, WorkspacePaused, "workspace.paused.v1"},
	{WorkspacePaused, WorkspaceCmdRestore, WorkspaceRestoring, "workspace.restoring.v1"},
	{WorkspaceRestoring, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
	// ‼️ RESTORING'S TERMINAL EDGE, and it was missing while its sibling had one. `recovering` could
	// always fail; `restoring` could only mark_ready, so a restore that could not finish left the
	// workspace in `restoring` FOREVER -- and `restoring` is not destroyable, so nothing could clear it
	// either. provision.go re-enters the same arm on every following message, takes the same error, and
	// tolerates the illegal transition: a permanent wedge that no operator surface could act on.
	//
	// The cost is not lost bytes -- the refusal is fail-closed and nothing is overwritten. It is
	// INDISTINGUISHABILITY: "restoring right now" and "this restore will never succeed" were the same
	// state, so no doctor could tell a slow resume from a dead one.
	{WorkspaceRestoring, WorkspaceCmdFail, WorkspaceFailed, "workspace.failed.v1"},

	{WorkspaceLeased, WorkspaceCmdLoseHost, WorkspaceHostLost, "workspace.host_lost.v1"},
	{WorkspaceHostLost, WorkspaceCmdRecover, WorkspaceRecovering, "workspace.recovering.v1"},
	{WorkspaceRecovering, WorkspaceCmdMarkReady, WorkspaceReady, "workspace.ready.v1"},
	{WorkspaceRecovering, WorkspaceCmdFail, WorkspaceFailed, "workspace.failed.v1"},

	{WorkspaceReady, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspacePaused, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspaceFailed, WorkspaceCmdDestroy, WorkspaceDestroying, "workspace.destroying.v1"},
	{WorkspaceDestroying, WorkspaceCmdFinishDestroy, WorkspaceDestroyed, "workspace.destroyed.v1"},
}
