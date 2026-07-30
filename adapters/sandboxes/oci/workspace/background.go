package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the container posture's implementation of toolbroker.BackgroundRunner. It is the sibling
// of exec.go's Run and, exactly like the host posture's background.go, it changes ONE thing about how
// the work is launched and nothing about how it is contained.

// The sandbox executor is a BackgroundRunner as well as a ShellRunner. Asserted here rather than in a
// test so that it is the compiler, not a tier someone has to remember to run, that keeps it true.
var _ toolbroker.BackgroundRunner = (*ShellExecutor)(nil)

// backgroundLabel marks a container as one of ours AND names the task it belongs to. It is two things at
// once on purpose: an operator hunting orphans greps for the key, and Kill proves ownership from the
// value being present at all. A container id is not reused the way a pid is, so this is a weaker hazard
// than the host posture's — but the RULE is the same rule and it is enforced with whatever evidence the
// posture has.
const backgroundLabel = "io.palai.bg"

// redirectForm is the container's command wrapper, and every character of it is load-bearing.
//
// The container writes its OWN log — the alternative was `docker logs`, which Docker's own documentation
// says is cached as unrotated JSON, and which would have been a second read path needing a second
// redaction path. So a shell is involved, and a shell is where an argv gets re-parsed.
//
// It does not get re-parsed here. In `sh -c '<script>' <arg0> <args...>` the shell binds arg0 to $0 and
// the rest to the POSITIONAL PARAMETERS, so `exec "$@" >"$0" 2>&1` expands exactly two things: the
// redirect target and the argv. A semicolon, a backtick, a `$(...)` or an `&&` inside any argument is a
// character in that argument and nothing else. That is a test (TestTheDetachedCommandFormInterpretsNo-
// Metacharacter), because it is the difference between this and the `strings.Join` the explicit Shell
// mode does above.
const redirectForm = `exec "$@" >"$0" 2>&1`

// Start launches one command in a hardened container that OUTLIVES this call, writing its merged output
// to a file inside the allocation. It returns the container id as the handle.
func (e *ShellExecutor) Start(ctx context.Context, cmd toolbroker.ShellCommand, spec toolbroker.BackgroundSpec) (toolbroker.Handle, error) {
	detached, ok := e.driver.(oci.DetachedDriver)
	if !ok {
		// A driver that cannot detach refuses rather than silently running the command in the foreground: a
		// model that asked for a background task and got a blocking call is blocked in exactly the way the
		// feature exists to prevent.
		return toolbroker.Handle{}, toolbroker.ErrBackgroundUnsupported
	}
	if len(cmd.Argv) == 0 {
		return toolbroker.Handle{}, errors.New("shell command requires an argv")
	}
	if cmd.WorkspaceRoot == "" {
		return toolbroker.Handle{}, errors.New("shell command requires a workspace root")
	}

	hostLog, err := spec.Resolve(cmd.WorkspaceRoot)
	if err != nil {
		return toolbroker.Handle{}, err
	}
	if err := prepareBackgroundLog(hostLog); err != nil {
		return toolbroker.Handle{}, err
	}

	runArgv := cmd.Argv
	if cmd.Shell {
		runArgv = []string{"/bin/sh", "-c", strings.Join(cmd.Argv, " ")}
	}
	// The in-container path of the same file. It is derived from the mount target rather than from the
	// host path, because the host path is never shown to the sandbox (spec §29.9).
	containerLog := path.Join(shellMountTarget, filepath.ToSlash(spec.OutputPath))
	wrapped := append([]string{"/bin/sh", "-c", redirectForm, containerLog}, runArgv...)

	// Layered on shellEnv() by the SAME call the synchronous path makes, so a colliding key is refused
	// before the container is created and the sandbox's own HOME/PATH cannot be shadowed.
	env, err := toolbroker.LayerEnv(shellEnv(), nil, cmd.Env)
	if err != nil {
		return toolbroker.Handle{}, err
	}

	ociSpec := oci.ContainerSpec{
		ImageDigest: e.image,
		Env:         env,
		Labels:      map[string]string{"io.palai.sandbox": "shell", backgroundLabel: spec.TaskID},
		Limits:      e.limits,
		// The output bounds are carried because ContainerSpec.validate requires them positive and the
		// detached path shares that validation deliberately. NOTHING READS THEM on this path: the container
		// writes its own file and the driver never attaches to its streams. The bound that matters for a
		// background task's output is the deadline the reaper enforces (E26 §0.2), which is what stops the
		// file growing rather than a byte ceiling on a stream nobody is reading.
		MaxStdoutBytes: e.maxStdoutBytes,
		MaxStderrBytes: e.maxStderrBytes,
		Cmd:            wrapped,
		WorkingDir:     shellMountTarget,
		Mounts:         []oci.Mount{{Source: cmd.WorkspaceRoot, Target: shellMountTarget, ReadOnly: cmd.ReadOnly}},
	}

	id, err := detached.StartDetached(ctx, ociSpec)
	if err != nil {
		_ = os.Remove(hostLog)
		return toolbroker.Handle{}, fmt.Errorf("sandbox background task: %w", err)
	}
	return toolbroker.Handle{Posture: toolbroker.PostureSandboxedLinux, Value: id}, nil
}

// Probe asks the DAEMON what became of a container. Everything it reports comes from there; nothing
// comes from memory this process holds, which is why it still answers after a restart.
func (e *ShellExecutor) Probe(ctx context.Context, handle toolbroker.Handle) (toolbroker.BackgroundStatus, error) {
	detached, containerID, err := detachedFor(e, handle)
	if err != nil {
		return toolbroker.BackgroundStatus{}, err
	}
	status, err := detached.InspectDetached(ctx, containerID)
	if err != nil {
		return toolbroker.BackgroundStatus{}, err
	}
	if !status.Found {
		// Removed. Terminal, and its exit status went with it — which is the honest NULL rather than a zero
		// that would read as a clean build.
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundExited}, nil
	}
	if status.Labels[backgroundLabel] == "" {
		// The container exists and is not ours. Same conclusion as a mismatched start time on the host
		// posture, and the same consequence: it is never signalled.
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundLost}, nil
	}
	if status.Running {
		return toolbroker.BackgroundStatus{State: toolbroker.BackgroundRunning}, nil
	}
	return toolbroker.BackgroundStatus{State: toolbroker.BackgroundExited, ExitCode: status.ExitCode}, nil
}

// Kill terminates and removes the container behind a handle, and REFUSES a container it cannot prove is
// ours. It is idempotent: a task already gone is a caller that lost a race.
func (e *ShellExecutor) Kill(ctx context.Context, handle toolbroker.Handle) error {
	detached, containerID, err := detachedFor(e, handle)
	if err != nil {
		return err
	}
	status, err := e.Probe(ctx, handle)
	if err != nil {
		return err
	}
	switch status.State {
	case toolbroker.BackgroundLost:
		return fmt.Errorf("%w: container %s carries no %s label", toolbroker.ErrHandleLost, containerID, backgroundLabel)
	case toolbroker.BackgroundExited:
		// It may be a STOPPED container rather than a removed one, and a stopped container still holds a
		// writable layer. KillDetached removes either, and is a no-op on one already gone.
		return detached.KillDetached(ctx, containerID)
	}
	return detached.KillDetached(ctx, containerID)
}

// detachedFor resolves the driver and validates the handle's posture. A host handle ("<pgid>:<time>")
// handed to the daemon would resolve to no container and read as "the task exited" — a WRONG ANSWER
// rather than an error — so the posture is checked rather than assumed.
func detachedFor(e *ShellExecutor, handle toolbroker.Handle) (oci.DetachedDriver, string, error) {
	detached, ok := e.driver.(oci.DetachedDriver)
	if !ok {
		return nil, "", toolbroker.ErrBackgroundUnsupported
	}
	if handle.Posture != toolbroker.PostureSandboxedLinux {
		return nil, "", fmt.Errorf("handle posture %q is not %q", handle.Posture, toolbroker.PostureSandboxedLinux)
	}
	if strings.TrimSpace(handle.Value) == "" {
		return nil, "", errors.New("background handle carries no container id")
	}
	return detached, handle.Value, nil
}

// prepareBackgroundLog creates .palai-session/bg/ and the task's log file on the HOST side, before the
// container exists. Two things make that necessary rather than tidy.
//
// FIRST, NOTHING ELSE CREATES THE DIRECTORY. workspace.Prepare makes repo/scratch/artifacts and the host
// executor makes home/tmp/simulators; the bg subtree belongs to neither. It sits under .palai-session,
// which Snapshot skips as a whole subtree, so a build log never enters a workspace snapshot or a
// checksum — which is why the log lives there and not in scratch/.
//
// SECOND, THE MODE. The container runs as the unprivileged uid 65532 and the allocation belongs to the
// control plane's uid, so the container cannot CREATE a file in it. Creating the file here and giving it
// 0o666 lets the container open the existing file for writing while the DIRECTORY stays 0o755 — the
// alternative, a world-writable directory, would let any local user drop files into a run's allocation.
// O_EXCL keeps a task id naming exactly one file: two processes interleaving into one log would be read
// by a model as one command's output.
func prepareBackgroundLog(hostLog string) error {
	if err := os.MkdirAll(filepath.Dir(hostLog), 0o755); err != nil {
		return fmt.Errorf("prepare background output directory: %w", err)
	}
	f, err := os.OpenFile(hostLog, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o666)
	if err != nil {
		return fmt.Errorf("open background output file: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("open background output file: %w", err)
	}
	// Explicit, because the process umask would otherwise strip the write bit the container needs.
	if err := os.Chmod(hostLog, 0o666); err != nil {
		return fmt.Errorf("open background output file to the sandbox uid: %w", err)
	}
	return nil
}
