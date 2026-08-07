package main

// `palai agentd install` — the ONE administrator action a device asks for, and the second and last verb
// this binary has.
//
// ‼️ IT INSTALLS THE PACKAGED DAEMON AND NEVER BUILDS ONE. The operator CLI's version of this resolved
// palai-agentd by running `go build ./cmd/palai-agentd` and refused when there was no source tree — the
// same defect §3.7 already deleted for the runner, standing one binary over. A fleet Mac has no checkout
// and no Go toolchain, so `accounts` isolation was unreachable on exactly the machines it exists for.
//
// The daemon travels in the device archive beside this binary (scripts/package/runner/build.sh), so the
// source is a sibling file: whatever `install.sh` extracted next to `palai`. That is also why the device
// needs nothing else on it — enrol, then this, and the machine can open per-session accounts.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"bytes"
	macosdeploy "github.com/palgroup/palai/deploy/macos"
	"github.com/palgroup/palai/packages/macagent"
)

// agentdSource is the packaged daemon beside this binary. It is resolved from the executable's own
// directory rather than from PATH: the archive puts them together, and a PATH lookup would find whichever
// palai-agentd an operator happened to have.
func agentdSource() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve this binary's path: %w", err)
	}
	source := filepath.Join(filepath.Dir(exe), "palai-agentd")
	if _, err := os.Stat(source); err != nil {
		return "", fmt.Errorf("no palai-agentd beside %s: %w\n"+
			"  the device archive carries it — extract palai-agentd from the same tarball `install.sh` "+
			"installed this binary from, put it here, and run this again", exe, err)
	}
	return source, nil
}

// installAgentd puts the account daemon on this machine and returns what it MEASURED off the socket,
// never what it attempted. It is the ONE install path: `palai agentd install` calls it, and so does
// `palai enroll`, because a device that enrols and cannot open a session account is a device that runs
// every customer's work as one uid.
func installAgentd(ctx context.Context, allocationRoot string) (macagent.Health, error) {
	source, err := agentdSource()
	if err != nil {
		return macagent.Health{}, err
	}
	plist, err := macosdeploy.LaunchDaemonPlist()
	if err != nil {
		return macagent.Health{}, fmt.Errorf("read the launch daemon description: %w", err)
	}
	// THE DAEMON LEARNS THE ALLOCATION ROOT FROM ITS SERVICE DEFINITION, which is where a launchd
	// service's configuration belongs and the only channel that survives: a root LaunchDaemon inherits
	// no shell, so an environment variable would arrive empty every time.
	//
	// It is the SAME value the agent was enrolled with, threaded here rather than re-derived, because
	// the two must name one directory: the control plane places allocations under <root>/slot-NN and
	// VerbAdopt gives <root>/slot-NN away. Two answers would be a chown of a directory nobody wrote to.
	plist = bytes.ReplaceAll(plist, []byte("__PALAI_ALLOCATION_ROOT__"), []byte(allocationRoot))
	return macagent.Install{
		Elevation: macagent.DetectElevation(ctx, os.Geteuid, macagent.ExecRunner),
		Run:       macagent.ExecRunner,
		Source:    source,
		Plist:     plist,
		Verify:    macagent.NewProber(macagent.DefaultSocketPath),
		TempDir:   os.TempDir(),
	}.Apply(ctx)
}

// runAgentdInstall is the explicit verb. It exists for a machine that was enrolled before enrolment
// installed the daemon, and for an operator re-running the install after replacing the binary.
func runAgentdInstall(ctx context.Context, out io.Writer) error {
	health, err := installAgentd(ctx, installedAllocationRoot())
	if errors.Is(err, macagent.ErrCannotElevate) {
		exe, _ := os.Executable()
		return fmt.Errorf("%w\n\n  run it once as root on this machine:\n      sudo %s agentd install", err, exe)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "palai-agentd installed and answering on %s (version %s, %d session account(s) open)\n",
		macagent.DefaultSocketPath, stampOrUnknown(health.Version), len(health.Slots))
	return nil
}

// runAgentdStatus is the read-only half: what is answering, right now, measured off the socket.
func runAgentdStatus(ctx context.Context, out io.Writer) error {
	health, err := macagent.NewProber(macagent.DefaultSocketPath).Probe(ctx)
	if err != nil {
		return fmt.Errorf("palai-agentd is not answering on %s: %w\n"+
			"  this machine can offer `user` isolation only — one customer, one uid, no cross-tenant boundary",
			macagent.DefaultSocketPath, err)
	}
	fmt.Fprintf(out, "palai-agentd answering on %s (version %s, %d session account(s) open)\n",
		macagent.DefaultSocketPath, stampOrUnknown(health.Version), len(health.Slots))
	return nil
}

// stampOrUnknown names the absence rather than printing an empty field: a daemon older than the version
// verb answers with no stamp, and "" beside a version label reads as a bug in the reader.
func stampOrUnknown(stamp string) string {
	if stamp == "" {
		return "unknown (older than the version verb)"
	}
	return stamp
}

// runAgentd dispatches `palai agentd <install|status>`. Two subcommands and no flags: an administrator
// action with options is an action somebody has to read a manual for.
func runAgentd(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: palai agentd <install|status>")
	}
	switch args[0] {
	case "install":
		return runAgentdInstall(ctx, out)
	case "status":
		return runAgentdStatus(ctx, out)
	default:
		return fmt.Errorf("unknown agentd subcommand %q — usage: palai agentd <install|status>", args[0])
	}
}

// installedAllocationRoot is the workspace root this machine was enrolled with, for the explicit
// `agentd install` verb. Enrolment passes its own value directly; this is the path for a machine that
// is re-installing the daemon after the fact, where the answer already lives in the config on disk.
func installedAllocationRoot() string {
	installed := installedDevice()
	if installed == nil {
		return ""
	}
	return workspaceRoot(installed)
}
