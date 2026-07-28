// Package host runs a workspace shell command DIRECTLY ON THIS MACHINE. It is the sibling of the
// OCI executor (adapters/sandboxes/oci/workspace/exec.go) and implements the same
// toolbroker.ShellRunner seam, with one difference that no comment should soften:
//
// THERE IS NO SANDBOX. The command runs as the control plane's own uid, in the control plane's own
// filesystem, with no container, no namespace, no cgroup bound and no network denial. The boundary
// is the uid — docs/research/macos-isolation-without-accounts.md measured (§2, 23 measurements) that
// under one uid nothing weaker is a boundary, Apple's SUPPORTED App Sandbox included. The operating
// rule that follows lives in docs/operations/palai-on-a-mac.md and is not enforceable here: different
// customers MUST use different Macs.
//
// What this package DOES keep from its OCI sibling, because those are properties of the result
// rather than of the container: bounded output (1 MiB stdout / 64 KiB stderr) with a truncation
// flag, secret redaction over the captured bytes, wall-time expiry classified as TimedOut, and a
// process-GROUP kill so a reaped `xcodebuild` does not leave a compiler behind. What it cannot keep
// is the mount: ReadOnly has no host equivalent, so a ReadOnly attempt is REFUSED rather than run
// writable under a read-only name.
package host

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ErrReadOnlyUnsupported refuses a read-only execution request on the host. The OCI executor honours
// ReadOnly with a read-only bind mount; there is no host equivalent of that mount, and running the
// command writable anyway would report a containment the machine does not have.
var ErrReadOnlyUnsupported = errors.New("read-only execution has no host equivalent: the native shell posture cannot bound writes, so the attempt is refused rather than run writable")

// envAllowList is the COMPLETE environment a host command receives. In the container the shell
// inherited nothing; on the host it would inherit the operator's own environment — SLACK_BOT_TOKEN,
// PALAI_GITHUB_APP_*, the master key, cloud credentials. So the environment is built from this list
// rather than filtered against a deny-list: a variable nobody thought of cannot arrive.
//
// Each entry earns its place: PATH finds the host's tools, HOME is where CoreSimulator keeps its
// device set and where toolchains cache, TMPDIR is the scratch space, LANG decides how tools encode
// their output, DEVELOPER_DIR selects the Xcode a `xcrun` resolves against.
var envAllowList = []string{"PATH", "HOME", "TMPDIR", "LANG", "DEVELOPER_DIR"}

// Executor runs one argv on the host and returns its bounded, redacted result. It implements
// toolbroker.ShellRunner.
type Executor struct {
	wallTime       time.Duration
	maxStdoutBytes int
	maxStderrBytes int
}

// NewExecutor binds the wall time a host command runs under (zero = unbounded, matching the OCI
// limits struct). Output bounds match the OCI executor's: 1 MiB stdout / 64 KiB stderr.
func NewExecutor(wallTime time.Duration) *Executor {
	return &Executor{wallTime: wallTime, maxStdoutBytes: 1 << 20, maxStderrBytes: 1 << 16}
}

// Run executes one argv on the host and returns its bounded, redacted result. A non-zero exit is a
// normal shell outcome (the command failed), not an executor error — including a missing command,
// which is reported as exit 127 the way a shell reports it, so the result reads the same under both
// postures. An error is returned only where the request itself is refused (no argv, no workspace
// root, ReadOnly) or the process could not be started at all.
func (e *Executor) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	if len(cmd.Argv) == 0 {
		return toolbroker.ShellResult{}, errors.New("shell command requires an argv")
	}
	if cmd.WorkspaceRoot == "" {
		return toolbroker.ShellResult{}, errors.New("shell command requires a workspace root")
	}
	if cmd.ReadOnly {
		return toolbroker.ShellResult{}, ErrReadOnlyUnsupported
	}

	// Same rule as the OCI executor: argv form runs the executable directly, so no metacharacter is
	// interpreted unless the caller opted into a shell.
	runArgv := cmd.Argv
	if cmd.Shell {
		runArgv = []string{"/bin/sh", "-c", strings.Join(cmd.Argv, " ")}
	}

	if e.wallTime > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, e.wallTime)
		defer cancel()
	}

	c := exec.CommandContext(ctx, runArgv[0], runArgv[1:]...)
	c.Dir = cmd.WorkspaceRoot
	c.Env = allowedEnv()
	// Setpgid puts the command in its own process group; Cancel kills that GROUP, so a wall-time
	// expiry reaps the tree a build spawns, not just the shell at its root. WaitDelay bounds the wait
	// on a descendant that still holds the output pipe open.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second

	stdout := &cappedBuffer{limit: e.maxStdoutBytes}
	stderr := &cappedBuffer{limit: e.maxStderrBytes}
	c.Stdout, c.Stderr = stdout, stderr

	start := time.Now()
	err := c.Run()
	result := toolbroker.ShellResult{
		Stdout:     toolbroker.RedactSecrets(stdout.String()),
		Stderr:     toolbroker.RedactSecrets(stderr.String()),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}

	switch {
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		// A shell reports a missing command as 127 rather than as a failure to run anything; the
		// container posture surfaces exactly that, and so does this one.
		result.ExitCode = 127
		result.Stderr = toolbroker.RedactSecrets(strings.TrimSpace(stderr.String() + "\n" + runArgv[0] + ": command not found"))
		return result, nil
	case err != nil && c.ProcessState == nil:
		return result, fmt.Errorf("host shell: %w", err)
	}

	result.ExitCode = c.ProcessState.ExitCode()
	if status, ok := c.ProcessState.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		// A signalled process has no exit code of its own; report it the way a shell does, and the
		// way the OCI executor classifies its own SIGKILL: 128 + signal.
		result.Signal = strings.TrimPrefix(status.Signal().String(), "signal: ")
		result.ExitCode = 128 + int(status.Signal())
	}
	// ctx.Err() outlives the kill: the deadline is what MADE the signal, so it is the reliable
	// classifier. OOMKilled stays false under every outcome — the host posture bounds no memory, and
	// claiming an OOM we did not observe would be worse than reporting none.
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		result.TimedOut = true
		result.Signal = "KILL"
	}
	return result, nil
}

// allowedEnv builds the command's environment from envAllowList. A variable absent from the control
// plane's own environment is simply absent from the command's — never defaulted, so the command sees
// the host as the operator configured it.
func allowedEnv() []string {
	env := make([]string, 0, len(envAllowList))
	for _, name := range envAllowList {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// cappedBuffer captures at most limit bytes and remembers that it dropped the rest. It always
// reports a full write, so a bounded reader never turns into a broken pipe the command can see:
// truncation is the caller's finding, not the command's failure.
type cappedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); room > 0 {
		if len(p) > room {
			b.buf.Write(p[:room])
			b.truncated = true
		} else {
			b.buf.Write(p)
		}
	} else if len(p) > 0 {
		b.truncated = true
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return b.buf.String() }
