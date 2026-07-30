package toolbroker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/packages/contracts"
)

// This file is the DETACHED half of the sandbox execution seam beside sandbox_exec.go's synchronous
// one, and it keeps that file's discipline exactly: the broker declares the seam a background-capable
// tool needs and owns NO mechanics. A process group, a container id, a start time and a signal all live
// in the two sandbox adapters; nothing in this package knows what a pgid is.
//
// WHAT MAKES THIS A SEPARATE INTERFACE RATHER THAN TWO MORE ShellRunner METHODS: a ShellRunner returns a
// RESULT and a BackgroundRunner returns a HANDLE, and every posture can implement the first while only
// some can implement the second. Kept apart, an executor that cannot detach simply is not a
// BackgroundRunner and the call is refused; folded together, it would have to stub two methods that lie.

// ErrBackgroundUnsupported reports an executor that runs commands but cannot detach one — the shell
// tool's background parameter is then refused rather than silently run in the foreground. A model that
// asked for a background task and got a blocking call would be blocked in exactly the way the feature
// exists to prevent, so a refusal is the honest answer and not a degraded one.
var ErrBackgroundUnsupported = errors.New("this shell posture cannot start a background task")

// ErrHandleLost reports a handle whose live counterpart CANNOT BE PROVEN TO BE OURS — on the host
// posture, a process occupying the recorded pgid whose start time does not match the one recorded at
// spawn. PID numbers are reused, so the occupant may be any process on the machine.
//
// THE RULE THIS ERROR EXISTS TO ENFORCE IS ABSOLUTE: a lost handle is never signalled. Not killed on a
// deadline, not killed on a run cancellation, not killed on a reaper sweep. The cost is our own orphan,
// which is a process that finishes its work and exits; the cost of the other choice is somebody else's
// process, which is a process that was doing something we know nothing about.
var ErrHandleLost = errors.New("background handle cannot be proven to name our own process")

// BackgroundSpec is what a background start needs BEYOND the command itself: the task id the row and
// the container label carry, and where the merged output goes.
//
// ShellCommand IS REUSED RATHER THAN EXTENDED, and that is a decision rather than a saving. E24
// measured ShellCommand's serialisability when it planned to send one over a relay to another machine;
// a field that only means something to a process on THIS machine would have cost that property for a
// future the relay still needs (E27). So the detachment lives in a second argument and the command
// stays the same command under both postures.
type BackgroundSpec struct {
	// TaskID is the background_tasks row id. It names the log file, labels the container
	// (io.palai.bg=<TaskID>), and is what an operator greps for when hunting an orphan.
	TaskID string
	// OutputPath is ALLOCATION-RELATIVE, matching the background_tasks.output_path column: the row stays
	// true if the allocation root moves, and nothing here discloses a host path (spec §29.9). Each posture
	// joins it to the root it already has — the host to cmd.WorkspaceRoot, the container to its mount
	// target — so the two never have to agree on a host path they cannot both see.
	OutputPath string
}

// Resolve joins OutputPath onto a root and refuses an escape. It lives here, on the spec, so BOTH
// postures get the same containment from the same six lines: a path check duplicated per adapter is a
// path check that will differ per adapter, and every path comparison this tree has shipped twice has
// been defeated once.
//
// The caller of a background start is the orchestrator, not the model — but "the caller is trusted" is
// the sentence that precedes every traversal, and the cost of not believing it is one filepath.Rel.
func (s BackgroundSpec) Resolve(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		return "", errors.New("background task requires a workspace root")
	}
	if strings.TrimSpace(s.TaskID) == "" {
		return "", errors.New("background task requires a task id")
	}
	if s.OutputPath == "" || filepath.IsAbs(s.OutputPath) {
		return "", fmt.Errorf("background output path %q must be relative to the allocation", s.OutputPath)
	}
	abs := filepath.Join(root, s.OutputPath)
	within, err := filepath.Rel(root, abs)
	if err != nil || within == ".." || strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("background output path %q escapes the allocation", s.OutputPath)
	}
	return abs, nil
}

// BackgroundPosture names which kind of operating-system object a Handle refers to. It is carried on
// the handle and stored in background_tasks.posture under a CHECK constraint, because the probe looks at
// a DIFFERENT object per posture and probing the wrong one does not error — it answers wrongly.
type BackgroundPosture string

const (
	// PostureSandboxedLinux: Handle.Value is a container id, and liveness is what the daemon says.
	PostureSandboxedLinux BackgroundPosture = "sandboxed-linux"
	// PostureUnsandboxedHost: Handle.Value is "<pgid>:<start time>", and liveness is a process probe on
	// this machine. The start time is the half that makes the pgid trustworthy.
	PostureUnsandboxedHost BackgroundPosture = "unsandboxed-host"
)

// BackgroundState is the lifecycle of a started process as the OPERATING SYSTEM reports it, never as our
// own bookkeeping remembers it. The distinction is the whole design: bookkeeping does not survive a
// restart and a container does.
type BackgroundState string

const (
	BackgroundRunning BackgroundState = "running"
	BackgroundExited  BackgroundState = "exited"
	// BackgroundLost — see ErrHandleLost. A probe returning this must never be followed by a signal.
	BackgroundLost BackgroundState = "lost"
)

// Handle is the durable name of one started process. It is a VALUE, not a live object: it is what a
// background_tasks row stores and what a control plane that just restarted has to work from, so it
// carries nothing a process could hold — no file descriptor, no *os.Process, no channel.
type Handle struct {
	Posture BackgroundPosture
	Value   string
}

// BackgroundStatus is one probe's answer. ExitCode is a POINTER because "not known" is a real answer and
// has no numeric spelling: a control plane that restarted knows a process is gone and cannot know what it
// returned. A sentinel here would become a -1 in a row and then a -1 a model compares against zero.
type BackgroundStatus struct {
	State    BackgroundState
	ExitCode *int
}

// BackgroundRunner starts a command that OUTLIVES the call that started it, and answers two questions
// about it afterwards. Three methods, no more: the orchestration (which run owns it, when it expires,
// who gets told it finished) is durable state above this seam, not mechanics behind it.
//
// Start must not block on the command. Probe must read the operating system rather than any memory Start
// left behind, because the process that ran Start may be gone. Kill is idempotent — killing a task twice
// is killing it once — and must send NO SIGNAL AT ALL for a handle whose probe reports BackgroundLost.
type BackgroundRunner interface {
	Start(ctx context.Context, cmd ShellCommand, spec BackgroundSpec) (Handle, error)
	Probe(ctx context.Context, handle Handle) (BackgroundStatus, error)
	Kill(ctx context.Context, handle Handle) error
}

// BackgroundTicket is what a background start hands back TO THE MODEL, and it is three fields because a
// fourth would be the output.
//
// THAT IS A CONTEXT DECISION AND IT IS THE POINT OF THE FEATURE. A model backgrounds a five-minute build
// precisely so it does not have to hold that build's output; returning the output anyway would spend the
// owner's money on the thing the call was made to avoid. Seeing it takes an explicit read of OutputPath —
// and no new tool for that read, because the output is a file and palai.workspace.file already reads
// files. The harness this replicates reached the same conclusion and deprecated its own TaskOutput tool
// "in favor of Read on the task's output file path" (E26 §3.5 P2).
type BackgroundTicket struct {
	// TaskID names the task in every later call about it — the kill tool's only argument.
	TaskID string
	// OutputPath is ALLOCATION-RELATIVE, so it is directly the `path` argument of a file read and it
	// discloses no host path (spec §29.9).
	OutputPath string
	// State is what the operating system said about the process a moment after it started, not what we
	// hoped: a command that has already failed reports `exited` rather than a comfortable `running`.
	State BackgroundState
}

// BackgroundTasks is the ORCHESTRATION seam a tool reaches background execution through, and it sits one
// layer above BackgroundRunner on purpose. A runner deals in handles, process groups and container ids;
// a tool deals in a task id and a path. Everything between the two — minting the id, deriving the log
// path, refusing the call when the deployment has switched the feature off, and knowing which run a task
// belongs to — is durable orchestration state, and the tool has no business holding any of it.
//
// It is carried on ExecEnv beside Shell/Tasks/Publications, the same way every other control-plane
// capability reaches a built-in tool. Nil on an attempt with nothing wired, and the tool then REFUSES
// rather than running the command in the foreground: a model that asked for a background task and got a
// blocking call is blocked in exactly the way the feature exists to prevent.
//
// StartBackground takes the CALL ID as an argument rather than reading it off the seam, because the seam
// is built once per dispatch and the id belongs to one call: the durable row a start writes carries a FK
// to the tool_calls row that spawned it (migration 000047's UNIQUE (tool_call_id)), and a task whose row
// named the wrong call would be a process no ledger entry accounts for. ExecEnv.CallID is where a tool
// reads it — the broker stamps it on a copy of the env before invoking Exec.
type BackgroundTasks interface {
	StartBackground(ctx context.Context, cmd ShellCommand, callID contracts.ToolCallID) (BackgroundTicket, error)
	KillBackground(ctx context.Context, taskID string) (BackgroundTicket, error)
}
