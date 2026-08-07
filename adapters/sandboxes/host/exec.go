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

	"github.com/palgroup/palai/packages/macagent"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ErrCannotDropPrivilege refuses a command that asked to run as a session account this process cannot
// become. It is SEPARATE from a malformed request (ErrRunAsOutsideNamespace) because the two are
// different findings about different things: one says the control plane sent a uid nobody should
// spend, the other says this machine was asked for a boundary it cannot build.
//
// ‼️ THE REFUSAL IS THE POINT. The alternative is to run the command as whoever this process already
// is, which is the operator's own uid on every Mac measured — and a tenant command running as uid 501
// under a request that named uid 707 is not a degraded boundary, it is a boundary that was REPORTED
// and does not exist. That is the exact shape docs/measurements/faz-a5-residue.md §2 found: an account
// minted, a home directory created, and every tenant still running as one principal.
var ErrCannotDropPrivilege = errors.New("this process cannot drop privilege to a session account")

// ErrRunAsOutsideNamespace refuses a uid that is not one palai-agentd allocates.
var ErrRunAsOutsideNamespace = errors.New("run-as uid is outside the session-account namespace")

// credentialFor turns a wire RunAs into the kernel credential a child is spawned with, or refuses.
//
// MEASURED ON THIS PLATFORM, 2026-08-05, Darwin 25.3.0, from a parent at uid 501:
//
//	Credential{Uid:701, NoSetGroups:true}   -> fork/exec: operation not permitted
//	Credential{Uid:self, NoSetGroups:true}  -> runs, child is 501
//	Credential{Uid:self, NoSetGroups:false} -> fork/exec: operation not permitted
//	os.Chown(f, 701, 20)                    -> operation not permitted
//
// Two things follow and both are in the code below. Changing to ANOTHER uid needs euid 0, so the check
// is on the process rather than on the request — no session account is reachable from an unprivileged
// execer, and the third line says even the identity credential is not, because Go calls setgroups()
// for a Credential and setgroups() always needs root.
//
// SUPPLEMENTARY GROUPS ARE DROPPED, WHICH IS WHY NoSetGroups IS LEFT FALSE. A root parent's group list
// includes wheel and admin on a Mac; NoSetGroups would make the child KEEP them, so a process that had
// just been given a tenant's uid would carry the supervisor's group memberships and the drop would give
// back most of what it took. Leaving it false makes Go call setgroups(0, NULL) with the nil Groups
// below, and the child ends with its uid, its gid, and nothing else.
func credentialFor(runAs *toolbroker.RunAs) (*syscall.Credential, error) {
	if runAs == nil {
		return nil, nil // no session-account layer: unchanged, and the identity is this process's own
	}
	// THE UID IS BOUNDED BEFORE ANYTHING IS SPENT ON IT. This value crossed a wire from a process the
	// tenant's model can influence; `Uid: 0` is expressible there and must not be expressible here.
	slot, ok := macagent.SlotFromUID(runAs.UID)
	if !ok {
		return nil, fmt.Errorf("%w: uid %d is not one palai-agentd allocates (%d..%d)",
			ErrRunAsOutsideNamespace, runAs.UID, macagent.UIDBase+1, macagent.UIDBase+macagent.MaxSlot)
	}
	// ‼️ THE GID IS CHECKED AGAINST THE SLOT THE UID NAMES, not against a fleet-wide constant. Each
	// session account now has its OWN group, so "is this a session gid" is no longer the question worth
	// asking — a uid from slot 07 arriving with slot 08's gid would pass that one and hand a tenant the
	// group bit on another tenant's workspace, which is precisely the boundary the per-slot group exists
	// to draw. The pair has to agree, and only the pair.
	want, err := macagent.SlotGID(slot)
	if err != nil {
		return nil, fmt.Errorf("%w: uid %d names slot %d, which has no gid: %v", ErrRunAsOutsideNamespace, runAs.UID, slot, err)
	}
	if runAs.GID != want {
		return nil, fmt.Errorf("%w: uid %d is slot %d, whose group is %d, and the request carries gid %d",
			ErrRunAsOutsideNamespace, runAs.UID, slot, want, runAs.GID)
	}
	return &syscall.Credential{Uid: uint32(runAs.UID), Gid: uint32(runAs.GID)}, nil
}

// procAttrFor is the SysProcAttr every command this package starts is given: the process group both
// postures have always set, plus the privilege drop when one was requested. It is ONE function for the
// synchronous and the background path because those two have drifted before — background.go's header
// records the one line that legitimately differs between them, and this is not it.
//
// ‼️ euid IS A PARAMETER AND NOT os.Geteuid(), FOR A TESTING REASON THAT IS ALSO THE POINT OF THE TEST.
// Every machine an agent works on is unprivileged, so a function that read the real euid could only
// ever be observed taking the REFUSING branch — and a change that dropped the Credential entirely would
// leave that refusal in place and every test green. Passing it makes both branches reachable from any
// machine, which is the difference between measuring the privilege drop and measuring the absence of
// privilege. macagent.DetectElevation takes geteuid the same way and for the same reason.
// ‼️ AND SINCE A.5 T4 THE DROP BRANCH FIRES ON NOTHING, WHICH IS A STATEMENT ABOUT THE DESIGN AND NOT
// AN ADMISSION THAT IT IS DEAD. Count the conditions rather than trusting the sentence: a non-nil
// Credential needs a non-nil RunAs, which the control plane only sends when a session-account layer is
// wired; spending it needs euid 0, and NO execer in this tree is uid 0 — this package runs as the
// operator on a Mac and inside the sandbox on Linux. Both halves have to hold and the second never
// does, so as of this commit the refusal below is what a request carrying a RunAs receives, every time,
// on every machine.
//
// THE DROP MOVED, IT WAS NOT REMOVED. cmd/palai-agentd is the one Palai process that is root, and its
// `spawn` verb starts a session worker AS palai-sNN, so the tenant's work arrives already at the right
// uid and there is nothing here left to drop. That is why neither this Credential nor a chown is on the
// working path any more, and it is why this code is kept rather than deleted: it is the arm that fires
// if an execer is ever handed a RunAs it cannot honour — a worker started at the wrong uid, or a
// posture wired to a plane the account layer does not reach. A refusal is the only correct answer
// there, because the alternative is running a tenant's command as whoever this process already is while
// a run record says otherwise, and that is the exact shape docs/measurements/faz-a5-residue.md §2
// found.
func procAttrFor(runAs *toolbroker.RunAs, euid int) (*syscall.SysProcAttr, error) {
	credential, err := credentialFor(runAs)
	if err != nil {
		return nil, err
	}
	if credential != nil && euid != 0 {
		return nil, fmt.Errorf("%w: this executor runs as uid %d and only uid 0 may become another; the "+
			"command was NOT run as %d and was not run as %d either — a session's work is meant to arrive "+
			"already at its own uid, started by palai-agentd's spawn verb, so reaching this line means the "+
			"process that ran it is not the session worker it should be",
			ErrCannotDropPrivilege, euid, runAs.UID, euid)
	}
	return &syscall.SysProcAttr{Setpgid: true, Credential: credential}, nil
}

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
// there as its cwd; these were the state it still shared with every other concurrent run.
//
//   - HOME — the POSIX home, and (until E22 T2) the OPERATOR's own ~/.ssh, ~/.aws and ~/.gitconfig.
//     Deriving it means the agent's `git` has no operator identity to commit as, which is the correct
//     outcome and is stated in docs/operations/palai-on-a-mac.md §5.
//   - TMPDIR — the scratch space. It must not fall back to /private/tmp, which is drwxrwxrwt
//     (docs/research/macos-isolation-without-accounts.md §6).
//   - PALAI_SIMCTL_SET — a per-run CoreSimulator device set for the agent to pass as
//     `simctl --set "$PALAI_SIMCTL_SET"`.
//   - PALAI_DERIVED_DATA — a per-run Xcode build tree for the agent to pass as
//     `xcodebuild -derivedDataPath "$PALAI_DERIVED_DATA"`. Same spelling scripts/ops/mac-sessions.sh
//     exports for the accounts posture, so an agent's argv reads the same on both.
//
// ‼️ HOME IS A POSIX REDIRECT AND NOTHING MORE, AND THIS COMMENT USED TO CLAIM OTHERWISE. It said HOME
// carried "toolchain caches, DerivedData"; MEASURED 2026-08-05 on Darwin 25.3.0 / Xcode 26.6, the
// DerivedData half is FALSE:
//
//	env HOME=<scratch> swift homeprobe.swift  -> NSHomeDirectory=/Users/salih  (NOT <scratch>),
//	                                             getenv(HOME)=<scratch>
//	env HOME=<scratch> xcodebuild -scheme … build  -> ** BUILD SUCCEEDED **, and
//	    ~/Library/Developer/Xcode/DerivedData went 0 -> 4 entries / 16296 KB in the OPERATOR's home
//	    while `find <scratch>` answered ONE line: the scratch directory itself.
//
// Foundation resolves `~` from the USER RECORD (getpwuid), not from $HOME. E22 X20 recorded one
// instance of this — HOME does not select the CoreSimulator device set — and explained it by
// CoreSimulatorService being a launchd-managed per-user XPC service the calling process's HOME never
// reaches. THE MECHANISM IS WIDER THAN THAT EXPLANATION: `xcodebuild` resolves DerivedData in-process
// with no XPC service anywhere, and lands in the login user's home just the same. So what HOME moves
// is `git`, `ssh` and every other dotfile-reading tool (measured the same day: a `git config --global`
// under a scratch HOME wrote <scratch>/.gitconfig and left the operator's untouched, 0 matches), and
// what it does not move is the Apple toolchain.
//
// THE LAST TWO ARE THEREFORE ADVISORY AND CANNOT BE OTHERWISE, which is why the proof is named
// TestSimctlSetIsAdvisoryNotEnforced rather than explained here: each is selected by an argv flag, and
// argv belongs to the model. The runner can offer a directory and can never require one. What it does
// buy is a REMOVER — both directories are inside the allocation, so IdleReleaser.release and
// WorkspaceRecovery reclaim them with the allocation (idle_release.go:182, workspace_recovery.go:227),
// which is more than the operator's ~/Library ever gets. The only thing that moves an Apple tool's
// output off a shared surface is a different uid: that is palai-agentd's session account, whose
// `delete <slot>` takes the account's whole ~/Library AND its /private/var/folders bucket.
var sessionDirs = [][2]string{
	{"HOME", "home"},
	{"TMPDIR", "tmp"},
	{"PALAI_SIMCTL_SET", "simulators"},
	{"PALAI_DERIVED_DATA", "derived"},
}

// SessionDirs reports the list above — variable name paired with its directory name — and
// SessionDirRoot reports the subdirectory they live under. Together they are the ALLOCATION-RELATIVE
// path of every surface this posture points a tenant's tools at.
//
// THEY EXIST FOR ONE CALLER AND THE REASON IS THE CALLER'S WHOLE POINT. Until 2026-08-05 the proof
// that a release leaves no tenant residue was a MANUAL measurement — one Mac, one marker, one pass
// (A.5 T4: 40 files under the marker before the cleanup, 0 after) — and nothing re-ran it. The guard
// that replaced it is apps/control-plane/internal/artifacts/residue_component_test.go, and it reads
// THIS list rather than restating it: a guard holding its own copy would go on reporting green the
// day a fifth entry is added here, which is precisely the failure ("a sweep that narrows silently
// still reports green") it was written to prevent. A copy is returned so the canonical slice cannot
// be rewritten through the accessor.
func SessionDirs() [][2]string {
	out := make([][2]string, len(sessionDirs))
	copy(out, sessionDirs)
	return out
}

// SessionDirRoot is the allocation subdirectory SessionDirs live under (".palai-session").
func SessionDirRoot() string { return sessionDir }

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

	// THE PRIVILEGE DROP IS RESOLVED BEFORE THE DIRECTORIES ARE MADE, because allowedEnv gives the ones
	// it creates to the same account and a refusal after a chown would leave a tree half handed over.
	procAttr, err := procAttrFor(cmd.RunAs, os.Geteuid())
	if err != nil {
		return toolbroker.ShellResult{}, err
	}

	env, err := allowedEnv(cmd.WorkspaceRoot, cmd.RunAs)
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
	// on a descendant that still holds the output pipe open. Credential, when this attempt named a
	// session account, is the uid it becomes — see procAttrFor.
	c.SysProcAttr = procAttr
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
		s = toolbroker.RedactValues(s, toolbroker.HostSecretValues())
		// And the CONTENTS of the files those variables point at: the paths are deliberately not masked,
		// but `cat`-ing one is how the same uid reaches key_local, the master key or the CA key.
		return toolbroker.RedactValues(s, toolbroker.HostSecretFileValues())
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
//
// AND THEY ARE GIVEN TO THE SESSION ACCOUNT HERE FOR THE SAME REASON THEY ARE CREATED HERE: nothing
// else knows they exist. execution.HandTreeTo hands over the allocation as the clone or the restore
// leaves it, and these three appear afterwards, made by this process and therefore owned by it. A HOME
// the command cannot write is the same failure as a HOME pointing nowhere, arriving later and reading
// as a broken toolchain.
func allowedEnv(workspaceRoot string, runAs *toolbroker.RunAs) ([]string, error) {
	env := make([]string, 0, len(envAllowList)+len(sessionDirs))
	for _, name := range envAllowList {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	// Lchown rather than Chown throughout, for the reason execution.HandTreeTo gives at length: Chown
	// follows a symlink and gives away its TARGET, and everything under the allocation root is reachable
	// by a model that already ran once in this tree.
	give := func(label, path string) error {
		if runAs == nil {
			return nil
		}
		if err := os.Lchown(path, runAs.UID, runAs.GID); err != nil {
			return fmt.Errorf("%w: giving per-session %s (%s) to uid %d: %v",
				ErrCannotDropPrivilege, label, path, runAs.UID, err)
		}
		return nil
	}
	// The PARENT is handed over too. MkdirAll below creates .palai-session on the way to the first
	// subdirectory, so it is this process's; a command that cannot write it cannot create the bg/
	// directory a background task needs (background.go), and that failure would arrive one seam away
	// from anything naming a uid.
	if err := os.MkdirAll(filepath.Join(workspaceRoot, sessionDir), 0o700); err != nil {
		return nil, fmt.Errorf("prepare per-session directory: %w", err)
	}
	if err := give(sessionDir, filepath.Join(workspaceRoot, sessionDir)); err != nil {
		return nil, err
	}
	for _, d := range sessionDirs {
		path := filepath.Join(workspaceRoot, sessionDir, d[1])
		if err := os.MkdirAll(path, 0o700); err != nil {
			return nil, fmt.Errorf("prepare per-session %s: %w", d[0], err)
		}
		if err := give(d[0], path); err != nil {
			return nil, err
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
