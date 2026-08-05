package toolbroker

import (
	"context"

	"github.com/palgroup/palai/packages/contracts"
)

// This file is the sandbox-backed execution seam behind the in-process broker (spec §28.7-28.8).
// The broker stays dependency-light: it defines the seam types a workspace-touching tool needs but
// owns no sandbox mechanics. The concrete runner (an OCI-driver-backed sandbox that mounts the
// workspace, drops all capabilities, disables the network, and bounds cgroup resources) lives in the
// oci adapter and is injected per attempt through ExecEnv.

// ExecEnv is the per-attempt context a control-plane-backed tool receives. A workspace-touching tool
// (file/shell) reads WorkspaceRoot/ReadOnly/Shell; a durable-registry tool (task/todo) reads
// Scope/Tasks. A pure conformance tool ignores it; a zero ExecEnv (no workspace/registry wired) makes
// such a tool fail cleanly rather than escape or touch the control plane's own state.
type ExecEnv struct {
	WorkspaceRoot string
	ReadOnly      bool
	Shell         ShellRunner
	// Workspace is the confined file/git surface the coding tools act through (A.3 T5). It is the
	// sibling of Shell and it is chosen the same way: from the connection this attempt holds, so an
	// allocation that lives on a machine is edited on that machine and one that lives here is edited
	// here. Nil on an attempt with no workspace bound — a workspace tool then answers `unavailable`
	// rather than reaching for this process's own filesystem.
	//
	// WorkspaceRoot IS STILL HERE AND STILL MEANS SOMETHING, which is worth saying because the two
	// look redundant. It is the root a SHELL COMMAND runs in (ShellCommand.WorkspaceRoot), a path in
	// the filesystem of whichever machine runs it, and it is also how a tool asks "is a workspace
	// bound at all". What it is no longer is a path this process may open.
	Workspace WorkspaceOps
	// Background is the orchestration seam a `background: true` shell call and the background kill tool
	// reach through (E26 T2). It is SEPARATE from Shell rather than a capability of it for the reason
	// BackgroundRunner is separate from ShellRunner: one answers with a result and the other with a
	// handle. Nil on an attempt with no background orchestration wired — the background parameter is then
	// refused, never silently downgraded to a synchronous run.
	Background BackgroundTasks
	// RunAs is the uid/gid this attempt's commands drop to, and RunAsUnresolved says the machine has a
	// session-account layer that could not name one.
	//
	// ‼️ THEY ARE TWO FIELDS BECAUSE THERE ARE THREE STATES AND ONE POINTER CAN ONLY HOLD TWO. A nil
	// RunAs with RunAsUnresolved false is "this deployment mints no accounts" — the declared
	// unsandboxed-host posture, every stack before A.5, and a legitimate configuration. A nil RunAs with
	// RunAsUnresolved TRUE is "this machine claims to isolate sessions and cannot name this one's uid",
	// which is the opposite of a legitimate configuration and is refused by tools/shell.go rather than
	// run. Collapsing the two into one nil is exactly how a machine that had stopped isolating anything
	// would keep answering tool calls as the operator's own uid, which is the defect A.5 exists to end.
	RunAs           *RunAs
	RunAsUnresolved bool
	// CallID and Fence are the per-call identity Execute stamps on a COPY of the env before invoking a
	// tool (never on the caller's template). A remote_http tool (E12 T4) keys its invoke Idempotency-Key
	// on CallID (a duplicate retry settles one server-side execution) and stamps Fence on the durable
	// async-operation row (the audit/reconcile bond to the ledger). An mcp tool (E12 T5) tags its advisory
	// tool_call.progress.v1 events with CallID so they correlate to the dispatched call (string(CallID)).
	// Pure/workspace tools ignore them.
	CallID contracts.ToolCallID
	Fence  uint64
	// Scope binds a durable task/todo operation to its tenant and session; Tasks is the durable
	// registry it persists through. Both zero on an attempt with no registry wired.
	Scope TaskScope
	Tasks TaskRegistry
	// Publications is the seam a side-effect tool (push/PR) records a pending publication + approval
	// through (spec §30.8). Nil on an attempt with no repository publication wired — the tool then
	// fails cleanly rather than acting.
	Publications PublicationRegistry
	// Artifacts is the object-store write-path a tool that produces a large body (web research) persists
	// the full bytes through (spec §22.6, T2), returning the artifact id. Nil on an attempt with no
	// object store wired — the tool then returns its bounded excerpt only, with an empty artifact id.
	Artifacts ArtifactWriter
	// EnvValues is the attempt's resolved ENVIRONMENT: the key→value pairs a shell command receives on
	// top of the sandbox's own closed environment (E25 T3). Empty on every attempt whose pinned revision
	// names no environment, which is every attempt in every deployment before E25 — so a shell command's
	// environment is then bit-identical to what it was.
	//
	// THIS FIELD HOLDS CREDENTIAL VALUES AND IS THE ONLY FIELD OF THIS STRUCT THAT DOES. Three rules
	// follow, and each is enforced somewhere rather than asserted here: the orchestrator fills it
	// IMMEDIATELY before the exec call and never earlier (so a run parked waiting for a human approval
	// holds no value); nothing writes it to the tool ledger, a ConfigSnapshot, an event or a log line;
	// and the executor masks its values in captured output (RedactValues) before the result is returned.
	//
	// Scope and expiry are the ATTEMPT itself — the run's answer to the worker pattern's
	// fence/scope/expiry triple (internal/workers/store.go RedeemSecretHandle) — FOR A SYNCHRONOUS CALL.
	//
	// THAT SENTENCE USED TO END "there is no handle to redeem and no deadline to check because the value's
	// whole life is one Execute call", AND E26 MEASURED IT FALSE (§0.4, §3.6 D9). It is corrected here
	// rather than left to rot, which is this tree's pattern (host/exec.go, workspace/exec.go) and the
	// failure E24 T8 found written in twenty-six places because nobody renewed it.
	//
	// A BACKGROUND START IS ALSO AN EXECUTE, and the process it starts OUTLIVES the call: a value handed
	// over here reaches exec.Cmd.Env (host) or container.Config.Env (oci) and then lives in the kernel's
	// environ copy for the whole life of that process. Killing the control plane does not take it back,
	// and no code change can make it otherwise. A test says so — the started command reads its own
	// environment at a moment the TEST releases, strictly after Start returned
	// (adapters/sandboxes/host: TestAnEnvironmentValueOutlivesTheExecuteCallThatHandedItOver).
	//
	// SO THE TRIPLE COMES BACK, and E26 T6 is where each part is answered for a task rather than for a
	// call: the SCOPE is still the pinned revision's key names, the EXPIRY is the task's own deadline_at
	// (a task carrying a value cannot be created without one), and the FENCE is the background_tasks row —
	// killable by id, killed by its run's cancellation, ended by the reaper. There is a handle to redeem
	// after all, and it is a row.
	//
	// ONE CONSEQUENCE LANDS BACK HERE: redaction of a background task's output cannot be the executor's,
	// because the process writes its own file rather than returning a captured string. It moved to the
	// READ path (execution.redactTaskOutput), which re-resolves those key names at read time.
	EnvValues map[string]string
}

// ArtifactWriter is the object-store write-path a body-producing tool persists through. It is
// STRUCTURALLY identical to execution.ArtifactWriter (the changeset compiler's seam), so the
// orchestrator's writer drops straight into ExecEnv. Primitive params keep the broker free of the
// artifacts package's types.
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, project, runID string, content []byte, mediaType, logicalType string, provenance map[string]any) (string, error)
}

// ShellRunner runs one argv command inside the sandbox and returns its captured, bounded result. The
// concrete implementation lives outside this package; the seam keeps the broker free of sandbox
// mechanics.
type ShellRunner interface {
	Run(ctx context.Context, cmd ShellCommand) (ShellResult, error)
}

// ShellCommand is one sandboxed execution request: the argv (never a shell string — the caller opts
// into a shell explicitly via Shell), the workspace root to mount, and whether it mounts read-only.
// Env is the attempt's environment values, layered ON TOP of whatever closed environment the executor
// builds for itself (LayerEnv). A colliding key is REFUSED, never overwritten, so the sandbox's own
// PATH/HOME cannot be shadowed by a key an operator named. Empty on every command before E25.
type ShellCommand struct {
	Argv          []string
	WorkspaceRoot string
	ReadOnly      bool
	Shell         bool
	Env           map[string]string
	// RunAs is the uid/gid this command must drop to before it runs. Nil means the executor's own
	// identity, which is what every command carried before A.5 T3 and what a deployment with no
	// session-account layer still carries.
	RunAs *RunAs
}

// RunAs is one session's kernel identity, crossing the wire from the control plane that minted the
// account to the machine that spawns the process.
//
// ‼️ IT CARRIES NO NAME, AND THE ABSENCE IS THE SECURITY PROPERTY — the same one packages/macagent's
// package comment builds its whole protocol around. The receiving side is about to spend privilege on
// these numbers, and it is reachable from a process the tenant's model can influence. A name field
// would be a string a compromised control plane could aim; two integers bounded to one namespace are
// not. The executor derives the account name back FROM the uid (macagent.SlotFromUID) when it needs
// one to report, so the only producer of a name is still arithmetic.
//
// THE NUMBERS ARE NOT TRUSTED EITHER. An executor that honours this MUST check the uid is inside the
// session-account namespace before spending anything on it (see adapters/sandboxes/host's
// credentialFor); `Uid: 0` is expressible on this wire and is refused where it lands, because a struct
// with two integers cannot make a bad integer unwriteable the way a missing field makes a name
// unwriteable.
type RunAs struct {
	UID int
	GID int
}

// ShellResult is the captured outcome of a sandboxed command: bounded, already-redacted output, the
// exit code / termination signal, and the resource usage the sandbox recorded.
type ShellResult struct {
	ExitCode   int
	Signal     string
	Stdout     string
	Stderr     string
	Truncated  bool
	TimedOut   bool
	OOMKilled  bool
	DurationMS int64
}
