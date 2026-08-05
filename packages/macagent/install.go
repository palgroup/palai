package macagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
)

// ErrCannotElevate is what an install returns when nothing here can raise privilege silently. It is a
// sentinel because the CALLER owns the sentence that follows it: only a composition root knows what this
// binary is called on this machine, and the one command an operator has to run names it.
var ErrCannotElevate = errors.New("palai-agentd is not installed and this process cannot install it")

// Install is one machine's zero-touch install of palai-agentd, and the reason it exists is the owner's
// condition: `palai up` on a fresh cloud Mac must need no second command, and a hundred machines cannot
// be typed at.
//
// THE PRIVILEGE IS ALREADY THERE ON EVERY MACHINE THIS IS FOR. EC2 Mac's ec2-user has passwordless sudo
// configured on first boot and runs a user-data script; MacStadium and Scaleway hand over an admin SSH
// user with a first-boot hook. So a bring-up on those machines installs its own daemon and continues. A
// laptop answers ElevationNone, and there the honest outcome is one command an operator runs once —
// never a prompt, and never a partial install.
type Install struct {
	// Elevation is how this install will run its privileged steps. ElevationNone REFUSES; see Apply.
	Elevation Elevation
	// Run is the exec boundary, an argv and never a command line. A field so a test can drive every
	// step, and assert which steps were NOT taken.
	Run Runner
	// Source is the palai-agentd build to install — a darwin binary this process can read.
	Source string
	// Plist is the launchd job description to write at [LaunchDaemonPlistPath]. It is bytes rather than
	// a path because the binary that installs may be a packaged one with no source tree behind it.
	Plist []byte
	// Group is the group whose membership is the credential, and Client the account to put in it — the
	// account the control plane runs as. Empty Client defaults to whoever is running this.
	Group  string
	Client string
	// Verify is the probe run AFTER the install, and it is not optional. Nothing here reports success on
	// a socket it has not spoken to.
	Verify *Prober
	// TempDir is where the plist is staged before a privileged copy puts it in place. Empty is the
	// system temp dir.
	TempDir string
}

// Apply installs the daemon and then MEASURES it.
//
// ‼️ IT REFUSES BEFORE IT ATTEMPTS. With no way to elevate, this returns [ErrCannotElevate] having run
// NOTHING — not a mkdir, not a copy, not a launchctl. A half-installed daemon is strictly worse than an
// absent one: an absent one is exactly what the enrolment gate is built to catch, while a binary in
// place with no job loaded, or a job loaded with no group to authorise anyone, is a machine that looks
// installed to every check that reads a path and answers nothing on the socket that matters.
//
// ‼️ AND IT VERIFIES BEFORE IT RETURNS. "I installed it" is a claim; "I connected and `list` answered"
// is a measurement, and the difference is this epic's entire reason for existing — the tree already
// shipped an install whose having-been-done was assumed and whose socket nothing ever dialled.
func (i Install) Apply(ctx context.Context) (Health, error) {
	if !i.Elevation.CanElevate() {
		return Health{}, fmt.Errorf("%w: neither root (euid %d) nor passwordless sudo (`%s -n %s` was refused)",
			ErrCannotElevate, os.Geteuid(), sudoPath, trueBinaryPath)
	}
	if i.Verify == nil {
		return Health{}, errors.New("an install with no probe behind it would report success on a socket nobody dialled")
	}
	if len(i.Plist) == 0 {
		return Health{}, errors.New("no launchd job description to install; a binary with no plist is a binary launchd never starts")
	}
	source, err := filepath.Abs(i.Source)
	if err != nil {
		return Health{}, fmt.Errorf("resolving the palai-agentd build to install: %w", err)
	}
	if fi, err := os.Stat(source); err != nil || fi.IsDir() {
		return Health{}, fmt.Errorf("the palai-agentd build to install is not a file at %s: %v", source, err)
	}
	group := i.Group
	if group == "" {
		group = DefaultGroup
	}
	client := i.Client
	if client == "" {
		if u, err := user.Current(); err == nil {
			client = u.Username
		}
	}
	if client == "" {
		return Health{}, errors.New("no account to authorise: group membership is the entire credential, and a group with no members is a daemon nothing can reach")
	}
	run := i.Run
	if run == nil {
		run = ExecRunner
	}
	step := func(name string, args ...string) error {
		bin, argv := i.Elevation.privileged(name, args...)
		if out, err := run(ctx, bin, argv...); err != nil {
			return fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(out))
		}
		return nil
	}

	// 1. The binary, root-owned and not writable by the account it protects against. `install` rather
	//    than a copy-then-chmod because it sets owner, group and mode in ONE step: a window where the
	//    file exists and is still writable by this uid is a window where the daemon root runs is a file
	//    this uid could have replaced.
	if err := step(installBinaryPath, "-d", "-o", "root", "-g", "wheel", "-m", "0755", filepath.Dir(InstalledBinaryPath)); err != nil {
		return Health{}, err
	}
	if err := step(installBinaryPath, "-o", "root", "-g", "wheel", "-m", "0755", source, InstalledBinaryPath); err != nil {
		return Health{}, err
	}

	// 2. The group, and then the one member. `-o read` first because `-o create` fails on a group that
	//    already exists, which is the ordinary case on every re-run and every upgrade — an install that
	//    is not idempotent is one an operator only ever runs successfully once.
	if _, err := run(ctx, dseditgroupPath, "-o", "read", group); err != nil {
		if err := step(dseditgroupPath, "-o", "create", "-r", "Palai agent clients", group); err != nil {
			return Health{}, err
		}
	}
	if err := step(dseditgroupPath, "-o", "edit", "-a", client, "-t", "user", group); err != nil {
		return Health{}, err
	}

	// 3. The job description, staged as this user and then put in place by a privileged copy. Staging
	//    matters: os.WriteFile to /Library/LaunchDaemons would need this PROCESS to be root, which is
	//    only one of the two elevations this supports, and a path that works under one and not the other
	//    is a path only half of the machines exercise.
	staged, err := i.stagePlist()
	if err != nil {
		return Health{}, err
	}
	defer os.Remove(staged)
	if err := step(installBinaryPath, "-o", "root", "-g", "wheel", "-m", "0644", staged, LaunchDaemonPlistPath); err != nil {
		return Health{}, err
	}

	// 4. Load it. `bootout` first and its failure IGNORED, because the two cases are indistinguishable
	//    and only one is a problem: on a first install there is no job to unload, and on an upgrade
	//    there is one still running the OLD binary. Bootstrapping over a loaded job fails, so skipping
	//    the unload would make every upgrade a no-op that reported success — the exact shape where a
	//    new file on disk and an old daemon on the socket disagree.
	bootoutBin, bootoutArgs := i.Elevation.privileged(launchctlPath, "bootout", "system/"+LaunchDaemonLabel)
	_, _ = run(ctx, bootoutBin, bootoutArgs...)
	if err := step(launchctlPath, "bootstrap", "system", LaunchDaemonPlistPath); err != nil {
		return Health{}, err
	}

	// 5. AND MEASURE IT. The probe retries, so the daemon binding its socket a moment after launchd
	//    forks it is not a failure — which is the same window a cold boot opens, met here on purpose.
	health, err := i.Verify.Probe(ctx)
	if err != nil {
		return health, fmt.Errorf("palai-agentd was installed and launchd was asked to start it, but its socket never answered: %w", err)
	}
	return health, nil
}

// stagePlist writes the job description somewhere this process can write, for the privileged copy to
// move into place. 0644 because the destination is world-readable anyway and a launchd job description
// is not a secret.
func (i Install) stagePlist() (string, error) {
	f, err := os.CreateTemp(i.TempDir, "palai-agentd-*.plist")
	if err != nil {
		return "", fmt.Errorf("staging the launchd job description: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(i.Plist); err != nil {
		os.Remove(f.Name())
		return "", fmt.Errorf("staging the launchd job description: %w", err)
	}
	if err := os.Chmod(f.Name(), 0o644); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
