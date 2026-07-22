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
}

// ArtifactWriter is the object-store write-path a body-producing tool persists through. It is
// STRUCTURALLY identical to execution.ArtifactWriter (the changeset compiler's seam), so the
// orchestrator's writer drops straight into ExecEnv. Primitive params keep the broker free of the
// artifacts package's types.
type ArtifactWriter interface {
	WriteArtifact(ctx context.Context, org, project, runID string, content []byte, mediaType, logicalType string, provenance map[string]any) (string, error)
}

// ShellRunner runs one argv command inside the sandbox and returns its captured, bounded result. The
// concrete implementation lives outside this package; the seam keeps the broker free of sandbox
// mechanics.
type ShellRunner interface {
	Run(ctx context.Context, cmd ShellCommand) (ShellResult, error)
}

// ShellCommand is one sandboxed execution request: the argv (never a shell string — the caller opts
// into a shell explicitly via Shell), the workspace root to mount, and whether it mounts read-only.
type ShellCommand struct {
	Argv          []string
	WorkspaceRoot string
	ReadOnly      bool
	Shell         bool
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
