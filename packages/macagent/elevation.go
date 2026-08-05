package macagent

import (
	"context"
	"os"
	"os/exec"
)

// InstalledBinaryPath is where the daemon binary lives on a machine, and it is the path the shipped
// plist's ProgramArguments names. An installer that copied a binary anywhere else would produce a
// machine where the copy succeeded and launchd still ran nothing — so the plist test asserts these are
// the same string rather than trusting two files to agree.
const InstalledBinaryPath = "/usr/local/libexec/palai-agentd"

// InstalledWorkerPath is the program [VerbSpawn] starts as a session account, and it is a CONSTANT for
// the same reason namePrefix in protocol.go is not overridable: this is the one thing a root daemon
// executes, and a settable one is a root daemon somebody can aim.
//
// ‼️ THE CALLER CANNOT NAME IT, CANNOT PREFIX IT AND CANNOT PASS IT A FLAG. That asymmetry is the whole
// safety argument for having a spawn verb at all — the caller supplies an integer, the daemon supplies
// the program — and it is why this is not a `-worker` flag beside `-socket` and `-group`. Those two
// bound who may REACH the daemon; this one decides what it RUNS, and the difference is that a wrong
// value for either of those is a daemon nobody can talk to, while a wrong value here is arbitrary code
// started by root at a uid of the caller's choosing.
//
// It is a SIBLING of the daemon rather than the same binary, and deliberately: palai-agentd is
// root-owned, root-run, and its verb set is the privilege. A worker is the opposite — it holds a
// tenant's work and runs as the tenant — and one binary being both is one binary whose argv decides
// which, which is a decision made in the place this package refuses to make decisions.
//
// ‼️ NOTHING INSTALLS IT YET, AND THAT IS STATED HERE RATHER THAN DISCOVERED. macagent.Install lays down
// the daemon, the group, the plist and the job; it does not lay down a worker, because no worker binary
// exists in this tree. A spawn on a machine today therefore fails at [SysadminctlAccounts.Spawn]'s
// executable check with a message naming this path. That refusal is the honest state of the mechanism:
// the account is real, the uid is real, the drop is real, and the program is not there.
const InstalledWorkerPath = "/usr/local/libexec/palai-session-worker"

// LaunchDaemonLabel is the job's label, and LaunchDaemonPlistPath where launchd reads it from. `launchctl
// bootout` takes the label, `bootstrap` takes the path, and an installer needs both.
const (
	LaunchDaemonLabel     = "net.pallasite.palai-agentd"
	LaunchDaemonPlistPath = "/Library/LaunchDaemons/" + LaunchDaemonLabel + ".plist"
)

// Absolute paths for every privileged command, and the reason is measured rather than stylistic: `sudo`
// resets PATH to sudoers' `secure_path`, and in this repository that already made ONE command run BSD
// `find` under sudo and `bfs` in a shell — a silent three-hour window in an ISO8601 stamp. An installer
// that runs half its steps through sudo and half not, resolving names each time, is that defect with
// root behind it.
const (
	sudoPath          = "/usr/bin/sudo"
	launchctlPath     = "/bin/launchctl"
	dseditgroupPath   = "/usr/sbin/dseditgroup"
	installBinaryPath = "/usr/bin/install"
	trueBinaryPath    = "/usr/bin/true"
)

// Elevation is how — or whether — this process can run a privileged command WITHOUT a human at the
// keyboard. It is a string rather than a bool because there are three answers and the middle one is the
// whole reason the zero-touch path works.
type Elevation string

const (
	// ElevationNone: neither root nor passwordless sudo. A hundred machines cannot be typed at, so this
	// is a REFUSAL to install rather than a prompt — a half-installed daemon is worse than an absent
	// one, because an absent one is what the enrolment gate is built to catch.
	ElevationNone Elevation = "none"
	// ElevationRoot: already uid 0. The MDM/pkg path, and a `sudo palai up` an operator ran themselves.
	ElevationRoot Elevation = "root"
	// ElevationPasswordlessSudo: `sudo -n` succeeds. This is the cloud path and it is not exotic — EC2
	// Mac's ec2-user has it configured on first boot, and MacStadium and Scaleway hand over an admin SSH
	// user. It is what makes `palai up` on a fresh cloud Mac need no second command.
	ElevationPasswordlessSudo Elevation = "passwordless-sudo"
)

// CanElevate reports whether an install may be ATTEMPTED at all.
func (e Elevation) CanElevate() bool { return e == ElevationRoot || e == ElevationPasswordlessSudo }

// Runner executes one argv and returns its combined output. It is an argv and never a command line: a
// privileged step composed as a string is a privileged step somebody can put a `;` in.
type Runner func(ctx context.Context, name string, args ...string) (string, error)

// ExecRunner is the production Runner.
func ExecRunner(ctx context.Context, name string, args ...string) (string, error) {
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err
}

// DetectElevation answers the question Step 2 of the plan asks: can I raise privilege with nobody
// watching?
//
// THE ORDER IS NOT COSMETIC. Being root is checked first because `sudo -n true` as root succeeds too,
// and reporting the sudo answer for a process that IS root would make every later command take a detour
// through a binary it does not need — one that rewrites PATH on the way.
//
// A FAILING `sudo -n` IS THE COMMON, CORRECT, UNINTERESTING CASE. It is what a laptop answers, and it is
// what THIS machine answered when the path was measured (Darwin 25.3.0, 2026-08-05: `sudo -n true` →
// exit 1, `sudo: a password is required`). It is not an error to report; it is the input to a decision.
func DetectElevation(ctx context.Context, geteuid func() int, run Runner) Elevation {
	if geteuid == nil {
		geteuid = os.Geteuid
	}
	if geteuid() == 0 {
		return ElevationRoot
	}
	if run == nil {
		run = ExecRunner
	}
	// -n is the whole point: never prompt. A sudo that asked would hang a bring-up driven by a cloud
	// provider's first-boot hook, where there is no terminal to answer it on.
	if _, err := run(ctx, sudoPath, "-n", trueBinaryPath); err == nil {
		return ElevationPasswordlessSudo
	}
	return ElevationNone
}

// privileged prefixes an argv with sudo when this process is not already root, so every step of an
// install is composed the same way and none of them can forget.
func (e Elevation) privileged(name string, args ...string) (string, []string) {
	if e == ElevationRoot {
		return name, args
	}
	return sudoPath, append([]string{"-n", name}, args...)
}
