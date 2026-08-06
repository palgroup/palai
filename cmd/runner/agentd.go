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

// runAgentdInstall puts the daemon on this machine and reports what it MEASURED, never what it attempted.
func runAgentdInstall(ctx context.Context, out io.Writer) error {
	source, err := agentdSource()
	if err != nil {
		return err
	}
	plist, err := macosdeploy.LaunchDaemonPlist()
	if err != nil {
		return fmt.Errorf("read the launch daemon description: %w", err)
	}

	probe := macagent.NewProber(macagent.DefaultSocketPath)
	health, err := macagent.Install{
		Elevation: macagent.DetectElevation(ctx, os.Geteuid, macagent.ExecRunner),
		Run:       macagent.ExecRunner,
		Source:    source,
		Plist:     plist,
		Verify:    probe,
		TempDir:   os.TempDir(),
	}.Apply(ctx)
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
