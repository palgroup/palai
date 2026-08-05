package stack

// agentd.go is the machine-level half of `palai up --native`: before this command puts a control plane
// and a runner on a Mac, it makes sure the thing that ISOLATES one tenant from the next is actually
// here, and installs it if it can do so with nobody watching.
//
// THE OWNER'S CONDITION IS THE WHOLE DESIGN. `palai up` on a cloud Mac must need no second command, and
// a hundred machines cannot be typed at. That is reachable because the privilege is already there on
// every machine this is for: EC2 Mac's ec2-user has passwordless sudo configured on first boot,
// MacStadium and Scaleway hand over an admin SSH user, and both run a first-boot hook. So the cloud path
// installs its own daemon and continues, and the laptop path — where nothing can elevate silently —
// REFUSES and prints the one command to run.
//
// WHAT IT WILL NOT DO IS PROMPT. A `sudo` without -n on a first-boot hook is a bring-up that hangs on a
// machine with no terminal to answer it, and a partial install is worse than none: an absent daemon is
// exactly what the enrolment gate catches, while a binary in place with no job loaded looks installed to
// every check that reads a path.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	macosdeploy "github.com/palgroup/palai/deploy/macos"
	"github.com/palgroup/palai/packages/macagent"
)

// AgentdStatus is what an ensure MEASURED. Every field came off the socket or off a command that ran;
// none of it is a claim that something was done.
type AgentdStatus struct {
	Health macagent.Health
	// Installed is true when THIS call put the daemon there or replaced it. It is separate from Health
	// because "it is answering" and "I made it answer" are different sentences and the report says which.
	Installed bool
	// Warnings are conditions that are not refusals. A version mismatch on a machine that cannot elevate
	// is the one that matters: the daemon works, it is simply not the build this binary ships with, and
	// refusing a machine that CAN isolate a session because a stamp drifted would fail closed on the
	// wrong fact.
	Warnings []string
}

// ensureAgentd is one ensure, with every boundary a field so a test drives the same code production
// does and can assert which privileged commands were NOT run.
type ensureAgentd struct {
	Probe     *macagent.Prober
	Elevation macagent.Elevation
	Run       macagent.Runner
	// Source is the palai-agentd build to install, and SourceVersion how to ask it what it is. An empty
	// Source means this binary has nothing to install and can only report.
	Source string
	Plist  []byte
	// SelfCommand is the ONE command an operator runs when nothing can elevate. It names this binary's
	// real path rather than the word `palai`, because the machine where this refusal is read is a machine
	// where nobody has yet decided what is on the PATH.
	SelfCommand string
	TempDir     string
}

// apply probes, decides, and measures again.
//
// THE ORDER IS THE POINT AND IT IS PROBE FIRST. An install driven without asking would reinstall on
// every bring-up, and on a machine that cannot elevate it would refuse one that was already working.
func (e ensureAgentd) apply(ctx context.Context) (AgentdStatus, error) {
	health, err := e.Probe.Probe(ctx)
	if err == nil {
		return e.reconcileVersion(ctx, health)
	}
	// A daemon that ANSWERED and refused is not a daemon to reinstall — it is one whose answer an
	// operator has to read. Only "nothing is there" leads to an install.
	if !errors.Is(err, macagent.ErrNotAnswering) {
		return AgentdStatus{Health: health}, err
	}
	installed, err := e.install(ctx)
	if err != nil {
		return AgentdStatus{Health: health}, err
	}
	return AgentdStatus{Health: installed, Installed: true}, nil
}

// install puts the daemon on this machine, or refuses without having attempted anything.
func (e ensureAgentd) install(ctx context.Context) (macagent.Health, error) {
	if e.Source == "" {
		return macagent.Health{}, fmt.Errorf("palai-agentd is not answering on %s and this binary has no build of it to install", e.Probe.SocketPath)
	}
	health, err := macagent.Install{
		Elevation: e.Elevation,
		Run:       e.Run,
		Source:    e.Source,
		Plist:     e.Plist,
		Verify:    e.Probe,
		TempDir:   e.TempDir,
	}.Apply(ctx)
	if errors.Is(err, macagent.ErrCannotElevate) {
		// THE REFUSAL CARRIES THE FIX, and it is one command rather than a procedure, because the thing
		// an operator does on a laptop is type once and never think about it again.
		return health, fmt.Errorf("%w\n\n        Run this once on this machine, then `palai up --native` again:\n            %s\n\n"+
			"        On a cloud Mac (EC2 Mac, MacStadium, Scaleway) the first-boot account already has passwordless sudo,\n"+
			"        so this installs itself and there is no second command.", err, e.SelfCommand)
	}
	return health, err
}

// reconcileVersion compares the daemon that is RUNNING with the build this binary would install.
//
// ‼️ THE TWO VERSIONS COME FROM TWO DIFFERENT PLACES AND ONLY ONE OF THEM IS AUTHORITY. The daemon's
// comes off the SOCKET; the candidate's comes from running the build with -version. They diverge in
// exactly the case worth catching: an upgrade that copied a new binary to InstalledBinaryPath and then
// failed to restart the job leaves a NEW FILE and an OLD DAEMON, and reading the file would report the
// upgrade as done. What decides whether an account gets created is what is answering, so that is what
// this compares.
//
// A MISMATCH IS NEVER A REFUSAL. With elevation it is an upgrade; without it, a warning. A machine whose
// daemon works and whose stamp drifted can still isolate a session, and failing closed on a stamp would
// take a sound machine out of the pool for a reason that has nothing to do with isolation — the exact
// "refused for the wrong cause" this task's probe exists to avoid one layer up.
func (e ensureAgentd) reconcileVersion(ctx context.Context, health macagent.Health) (AgentdStatus, error) {
	// A daemon too old to name itself leaves this empty, and nothing is compared — so nothing is EXECUTED
	// either. Asking the candidate first would spend a fork on every bring-up to reach a comparison that
	// was already known to be impossible.
	if health.Version == "" {
		return AgentdStatus{Health: health}, nil
	}
	want := e.candidateVersion(ctx)
	if want == "" || want == health.Version {
		return AgentdStatus{Health: health}, nil
	}
	if !e.Elevation.CanElevate() {
		return AgentdStatus{Health: health, Warnings: []string{fmt.Sprintf(
			"palai-agentd on the socket reports %q and this binary ships %q; nothing here can elevate silently, so it was left alone. Upgrade with: %s",
			health.Version, want, e.SelfCommand)}}, nil
	}
	upgraded, err := e.install(ctx)
	if err != nil {
		return AgentdStatus{Health: health}, fmt.Errorf("upgrading palai-agentd from %q to %q: %w", health.Version, want, err)
	}
	return AgentdStatus{Health: upgraded, Installed: true}, nil
}

// candidateVersion asks the build this binary would install what it is, by RUNNING it — the same
// question the socket answers, put to the other half. An empty answer means the two cannot be compared,
// and nothing is upgraded on the strength of a comparison that could not be made.
func (e ensureAgentd) candidateVersion(ctx context.Context) string {
	if e.Source == "" {
		return ""
	}
	run := e.Run
	if run == nil {
		run = macagent.ExecRunner
	}
	out, err := run(ctx, e.Source, "-version")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// EnsureAgentd is the production ensure `palai up --native` drives.
func EnsureAgentd(ctx context.Context, p paths) (AgentdStatus, error) {
	plist, err := macosdeploy.LaunchDaemonPlist()
	if err != nil {
		return AgentdStatus{}, fmt.Errorf("the launchd job description this binary carries is unreadable: %w", err)
	}
	// A build to install is resolved BEST-EFFORT. Outside a checkout with no PALAI_AGENTD_BIN there is
	// nothing to install, and that is a refusal with a different sentence — not a reason to fail before
	// the probe has even run, since the daemon may well already be there.
	source, _ := agentdBinary(p)
	return ensureAgentd{
		Probe:       macagent.NewProber(macagent.DefaultSocketPath),
		Elevation:   macagent.DetectElevation(ctx, os.Geteuid, macagent.ExecRunner),
		Run:         macagent.ExecRunner,
		Source:      source,
		Plist:       plist,
		SelfCommand: selfInstallCommand(),
	}.apply(ctx)
}

// agentdBinary resolves the palai-agentd build to install, the same way nativeBinary resolves the
// control plane's: named by PALAI_AGENTD_BIN, or BUILT from a checkout.
func agentdBinary(p paths) (string, error) {
	if bin := strings.TrimSpace(os.Getenv("PALAI_AGENTD_BIN")); bin != "" {
		if _, err := os.Stat(bin); err != nil {
			return "", fmt.Errorf("PALAI_AGENTD_BIN=%s: %w", bin, err)
		}
		return bin, nil
	}
	root, fromSource := buildContext(p.compose)
	if !fromSource {
		return "", errors.New("there is no source tree here to build palai-agentd from — fix: set PALAI_AGENTD_BIN to a darwin build of ./cmd/palai-agentd")
	}
	out := filepath.Join(p.home, "bin", "palai-agentd")
	if err := os.MkdirAll(filepath.Dir(out), 0o700); err != nil {
		return "", err
	}
	cmd := exec.Command("go", "build", "-o", out, "./cmd/palai-agentd")
	cmd.Dir = root
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("build palai-agentd: %w", err)
	}
	return out, nil
}

// selfInstallCommand is the one command an operator runs on a machine that cannot elevate silently. It
// names this binary's resolved path, because a message saying `palai` is a message that is wrong on
// every machine where it is not yet on the PATH — which is every machine reading this refusal.
func selfInstallCommand() string {
	exe, err := os.Executable()
	if err != nil || exe == "" {
		return "sudo palai agentd install"
	}
	return "sudo " + exe + " agentd install"
}

// AgentdInstall is `palai agentd install`: the one command the refusal above names. It is the same
// ensure the bring-up drives, run on its own, so the path an operator types and the path a cloud Mac
// takes are one code path rather than two that have to agree.
func AgentdInstall() error {
	_, p, err := loadConfig()
	if err != nil {
		return err
	}
	status, err := EnsureAgentd(context.Background(), p)
	if err != nil {
		return err
	}
	for _, w := range status.Warnings {
		fmt.Fprintf(os.Stderr, "WARNING %s\n", w)
	}
	fmt.Fprintf(os.Stderr, "%s\n", agentdLine(status))
	return nil
}

// AgentdStatusReport is `palai agentd status`: probe and say what answered, without installing anything.
func AgentdStatusReport() error {
	health, err := macagent.NewProber(macagent.DefaultSocketPath).Probe(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "palai-agentd on %s: version %s, %d session account(s) open\n",
		macagent.DefaultSocketPath, stampOrUnknown(health.Version), len(health.Slots))
	return nil
}

// agentdLine is the operator-facing sentence the bring-up report carries. It says what was MEASURED,
// including the cold-boot race when it was lost and recovered from — a silent success there is
// indistinguishable from a machine that was already up, and the difference is the one thing this task
// bought.
func agentdLine(status AgentdStatus) string {
	verb := "already running"
	if status.Installed {
		verb = "installed and verified"
	}
	line := fmt.Sprintf("palai-agentd %s on %s (version %s, %d session account(s) open)",
		verb, macagent.DefaultSocketPath, stampOrUnknown(status.Health.Version), len(status.Health.Slots))
	if status.Health.Attempts > 1 {
		line += fmt.Sprintf(" — its socket refused %d time(s) first and answered after %s, which is the cold-boot window",
			status.Health.Attempts-1, status.Health.Waited)
	}
	return line
}

func stampOrUnknown(stamp string) string {
	if stamp == "" {
		return "unknown (older than the version verb)"
	}
	return stamp
}
