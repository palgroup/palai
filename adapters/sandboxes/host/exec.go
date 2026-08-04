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
	"path/filepath"
	"strings"
	"syscall"
	"time"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ErrReadOnlyUnsupported refuses a read-only execution request on the host. The OCI executor honours
// ReadOnly with a read-only bind mount; there is no host equivalent of that mount, and running the
// command writable anyway would report a containment the machine does not have.
var ErrReadOnlyUnsupported = errors.New("read-only execution has no host equivalent: the native shell posture cannot bound writes, so the attempt is refused rather than run writable")

// envAllowList is what a host command INHERITS from the control plane. In the container the shell
// inherited nothing; on the host it would inherit the operator's own environment — SLACK_BOT_TOKEN,
// PALAI_GITHUB_APP_*, the master key, cloud credentials. So the environment is built from this list
// rather than filtered against a deny-list: a variable nobody thought of cannot arrive.
//
// Each entry earns its place: PATH finds the host's tools, LANG decides how tools encode their
// output, DEVELOPER_DIR selects the Xcode a `xcrun` resolves against.
//
// TOGETHER WITH sessionDirs BELOW, THESE WERE THE COMPLETE ENVIRONMENT UNTIL E25 T3, AND THAT
// SENTENCE STOOD HERE VERBATIM UNTIL IT STOPPED BEING TRUE. There is now exactly one other source and
// it is a CALLER-SUPPLIED LAYER, never an inheritance: ShellCommand.Env carries the attempt's own
// environment values (an operator-authored environment, resolved from secret_refs at exec time) and
// toolbroker.LayerEnv adds them ON TOP of what allowedEnv builds. Nothing in this file's two lists can
// be overwritten by that layer — a colliding key is refused and the command does not run — so the
// property the allow-list exists for is unchanged: a variable nobody named cannot ARRIVE, and a
// variable this file derived cannot be REPLACED. What changed is that a variable an operator named
// explicitly, for one environment, can now be present.
var envAllowList = []string{"PATH", "LANG", "DEVELOPER_DIR"}

// sessionDir is the allocation subdirectory holding a run's own machine-local state. It is
// deliberately NOT workspace content — see the Exclusions rule in the oci workspace package, which
// keeps it out of every snapshot; a captured simulator device set would be gigabytes of a run's
// scratch masquerading as its repository.
const sessionDir = ".palai-session"

// sessionDirs are the variables the runner DERIVES from the allocation rather than inheriting, one
// directory each under <allocation>/.palai-session. Every run already got its own allocation and ran
// there as its cwd; these three were the state it still shared with every other concurrent run.
//
//   - HOME — toolchain caches, DerivedData, and (until E22 T2) the OPERATOR's own ~/.ssh, ~/.aws and
//     ~/.gitconfig. Deriving it means the agent's `git` has no operator identity to commit as, which
//     is the correct outcome and is stated in docs/operations/palai-on-a-mac.md §5.
//   - TMPDIR — the scratch space. It must not fall back to /private/tmp, which is drwxrwxrwt
//     (docs/research/macos-isolation-without-accounts.md §6).
//   - PALAI_SIMCTL_SET — a per-run CoreSimulator device set for the agent to pass as
//     `simctl --set "$PALAI_SIMCTL_SET"`.
//
// THE LAST ONE IS ADVISORY AND CANNOT BE OTHERWISE, which is why the proof is named
// TestSimctlSetIsAdvisoryNotEnforced rather than explained here. MEASURED 2026-07-28 (E22 X20): HOME
// does NOT select the device set — `simctl` is a thin client and the set belongs to a launchd-managed
// per-user CoreSimulatorService already running under the login session's HOME. The set is selected
// by an argv flag, and argv belongs to the model.
var sessionDirs = [][2]string{
	{"HOME", "home"},
	{"TMPDIR", "tmp"},
	{"PALAI_SIMCTL_SET", "simulators"},
}

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

// WallTime reports the bound this executor was built with, so a composition root can be asked what it
// actually wired. It exists because zero is not a neutral value here — it is UNBOUNDED, and an
// unbounded host executor is indistinguishable from a bounded one until a command hangs.
func (e *Executor) WallTime() time.Duration { return e.wallTime }

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

	env, err := allowedEnv(cmd.WorkspaceRoot)
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	// The attempt's own environment values, layered on top. A refused key returns BEFORE the process is
	// started: a command that ran under a caller-supplied PATH and reported an error afterwards would
	// already have executed the caller's binaries.
	env, err = toolbroker.LayerEnv(env, reservedEnvNames(), cmd.Env)
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	// The values this attempt carries, masked in whatever the command writes. Computed once here rather
	// than per output stream, and used for BOTH streams below — an accidental echo is as likely on stderr.
	envValues := toolbroker.EnvValueList(cmd.Env)

	c := exec.CommandContext(ctx, runArgv[0], runArgv[1:]...)
	c.Dir = cmd.WorkspaceRoot
	c.Env = env
	// Setpgid puts the command in its own process group; Cancel kills that GROUP, so a wall-time
	// expiry reaps the tree a build spawns, not just the shell at its root. WaitDelay bounds the wait
	// on a descendant that still holds the output pipe open.
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second

	stdout := &cappedBuffer{limit: e.maxStdoutBytes}
	stderr := &cappedBuffer{limit: e.maxStderrBytes}
	c.Stdout, c.Stderr = stdout, stderr

	// redact runs BOTH redactors over every captured byte: the shape-based one (provider keys, bearer
	// tokens) and the value-based one (this attempt's own environment values). Both, because neither sees
	// what the other does — RedactSecrets cannot recognise an operator's database password, and
	// RedactValues cannot recognise a token the agent obtained some other way.
	//
	// AND A THIRD SOURCE, which is not about this attempt at all: the control plane's OWN secret-named
	// environment values. The child environment is a three-name allow-list, so an agent cannot INHERIT a
	// secret — but on this posture it runs as the same uid as the control plane and can READ one.
	// Measured 2026-08-04: `ps -E -p <control-plane pid>` served 62 variables with values, and
	// `os.Unsetenv` does not remove them (macOS answers `ps` from the kernel's copy of the initial
	// environment). Masking the value closes every path that carries it, `cat .palai/api-key` included.
	redact := func(s string) string {
		s = toolbroker.RedactValues(toolbroker.RedactSecrets(s), envValues)
		return toolbroker.RedactValues(s, toolbroker.HostSecretValues())
	}

	start := time.Now()
	err = c.Run()
	result := toolbroker.ShellResult{
		Stdout:     redact(stdout.String()),
		Stderr:     redact(stderr.String()),
		Truncated:  stdout.truncated || stderr.truncated,
		DurationMS: time.Since(start).Milliseconds(),
	}

	switch {
	case errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist):
		// A shell reports a missing command as 127 rather than as a failure to run anything; the
		// container posture surfaces exactly that, and so does this one.
		result.ExitCode = 127
		result.Stderr = redact(strings.TrimSpace(stderr.String() + "\n" + runArgv[0] + ": command not found"))
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

// allowedEnv builds the command's environment: envAllowList inherited from the control plane, plus
// sessionDirs derived from this run's own allocation. A variable absent from the control plane's own
// environment is simply absent from the command's — never defaulted, so the command sees the host as
// the operator configured it.
//
// The session directories are created here rather than at allocation time because this is the only
// posture that has them: an OCI run gets its separation from the container. A directory that could
// not be created is an ERROR, not a silently-inherited fallback — a HOME pointing nowhere is worse
// than the shared one it replaced.
func allowedEnv(workspaceRoot string) ([]string, error) {
	env := make([]string, 0, len(envAllowList)+len(sessionDirs))
	for _, name := range envAllowList {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	for _, d := range sessionDirs {
		path := filepath.Join(workspaceRoot, sessionDir, d[1])
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("prepare per-session %s: %w", d[0], err)
		}
		env = append(env, d[0]+"="+path)
	}
	return env, nil
}

// reservedEnvNames is every variable name THIS POSTURE CLAIMS, whether or not it populated one. It is
// derived from the same two slices allowedEnv builds from, so a variable added to either list is
// reserved by that edit alone.
//
// IT IS SEPARATE FROM allowedEnv'S OUTPUT BECAUSE THE TWO SETS DIFFER, and the difference was measured
// (2026-07-30, E25 T3): allowedEnv uses os.LookupEnv, so a name the control plane does not carry never
// appears among its entries. On a Mac with no exported DEVELOPER_DIR the environment it returns has no
// DEVELOPER_DIR — and an environment key by that name was accepted, which would have chosen the Xcode
// the agent's `xcrun` resolved against. "Not populated" is not "not claimed".
func reservedEnvNames() []string {
	names := make([]string, 0, len(envAllowList)+len(sessionDirs))
	names = append(names, envAllowList...)
	for _, d := range sessionDirs {
		names = append(names, d[0])
	}
	return names
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
