package stack

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	macosdeploy "github.com/palgroup/palai/deploy/macos"
	"github.com/palgroup/palai/packages/macagent"
)

// TestWithoutSilentElevationTheInstallIsRefusedRatherThanAttempted.
//
// ‼️ THE ASSERTION IS THAT NOTHING RAN. "Refused rather than attempted" is not about the message — it is
// about the machine being left exactly as it was found. A half-installed daemon is strictly WORSE than
// an absent one: an absent one is precisely what the enrolment gate catches, while a binary in place
// with no launchd job, or a job loaded with an empty group, is a machine that looks installed to every
// check that reads a path and answers nothing on the socket that decides anything.
//
// AND IT REFUSES RATHER THAN PROMPTING. `sudo` without -n on a cloud provider's first-boot hook is a
// bring-up that hangs on a machine with no terminal to answer it; there are a hundred of these and
// nobody is at any of them.
//
// This is the LAPTOP path, and it is the path THIS machine takes — measured 2026-08-05 on Darwin
// 25.3.0: `sudo -n /usr/bin/true` exits 1 with `sudo: a password is required`, and euid is 501.
func TestWithoutSilentElevationTheInstallIsRefusedRatherThanAttempted(t *testing.T) {
	t.Run("nothing privileged is run, and the refusal carries the one command", func(t *testing.T) {
		ran := &recorder{}
		// The REAL detection, driven the way production drives it: a non-root euid and a sudo that
		// refuses. Nothing here decides the elevation on the test's behalf.
		ran.fail = map[string]bool{"/usr/bin/sudo": true}
		ensure := ensureFixture(t, ran)
		ensure.Elevation = macagent.DetectElevation(context.Background(), func() int { return 501 }, ran.run)
		if ensure.Elevation != macagent.ElevationNone {
			t.Fatalf("the fixture did not reproduce a machine that cannot elevate (got %q), so this test measures nothing", ensure.Elevation)
		}

		_, err := ensure.apply(context.Background())
		if err == nil {
			t.Fatal("a machine that cannot elevate silently reported a successful install")
		}
		if !errors.Is(err, macagent.ErrCannotElevate) {
			t.Errorf("the refusal does not carry ErrCannotElevate: %v", err)
		}
		if !strings.Contains(err.Error(), ensure.SelfCommand) {
			t.Errorf("the refusal does not name the one command to run (%q): %v", ensure.SelfCommand, err)
		}

		// THE MEASUREMENT. Every privileged verb an install would use, and none of them may appear.
		for _, forbidden := range []string{"/usr/bin/install", "/usr/sbin/dseditgroup", "/bin/launchctl"} {
			if ran.sawBinary(forbidden) {
				t.Errorf("the install ATTEMPTED %s on a machine it could not elevate on; a half-installed daemon is worse than none.\nran: %v",
					forbidden, ran.commands)
			}
		}
		// And nothing landed on disk either: the plist is staged before the privileged copy, and a
		// staged file left behind is a file the next run would find.
		if entries, err := os.ReadDir(ensure.TempDir); err != nil || len(entries) != 0 {
			t.Errorf("the refusal left %d file(s) in the staging directory (err %v); it should have written nothing at all", len(entries), err)
		}
	})

	t.Run("with root the SAME ensure installs, and then dials the socket before reporting success", func(t *testing.T) {
		// Without this the test above passes against an ensure that refuses everything, which is an
		// ensure nobody could ship — and it is where the "installed" claim becomes a measurement.
		ran := &recorder{}
		ensure := ensureFixture(t, ran)
		ensure.Elevation = macagent.ElevationRoot
		// The daemon appears when launchd is asked to start the job, which is the sequence a real
		// machine runs: bootstrap, then a socket that answers a moment later.
		ran.onBootstrap = func() { startStubDaemon(t, ensure.Probe.SocketPath, "9.9.9") }

		status, err := ensure.apply(context.Background())
		if err != nil {
			t.Fatalf("install: %v", err)
		}
		if !status.Installed {
			t.Error("the ensure reported the daemon as already running when it had just installed it")
		}
		if status.Health.Version != "9.9.9" {
			t.Errorf("Health.Version = %q: the success was reported without the socket having answered", status.Health.Version)
		}
		for _, want := range []string{"/usr/bin/install", "/usr/sbin/dseditgroup", "/bin/launchctl"} {
			if !ran.sawBinary(want) {
				t.Errorf("the install never ran %s.\nran: %v", want, ran.commands)
			}
		}
		// The binary and the job description have to land where the plist and launchd look, or the copy
		// succeeds and launchd still runs nothing.
		if !ran.sawArg(macagent.InstalledBinaryPath) {
			t.Errorf("nothing was installed at %s, which is the path the plist's ProgramArguments names", macagent.InstalledBinaryPath)
		}
		if !ran.sawArg(macagent.LaunchDaemonPlistPath) {
			t.Errorf("no job description was installed at %s", macagent.LaunchDaemonPlistPath)
		}
		// EVERY privileged step is an argv. A command line here is a command line somebody puts a `;` in.
		for _, c := range ran.commands {
			for _, arg := range c {
				if strings.ContainsAny(arg, ";|&$") {
					t.Errorf("a privileged step carried shell metacharacters in %q", c)
				}
			}
		}
	})

	t.Run("an install whose socket never answers is a FAILURE, not a success", func(t *testing.T) {
		// "I installed it" is a claim. This repository already shipped an install whose having-been-done
		// was assumed and whose socket nothing ever dialled, which is why this phase exists.
		ran := &recorder{}
		ensure := ensureFixture(t, ran)
		ensure.Elevation = macagent.ElevationRoot

		_, err := ensure.apply(context.Background())
		if err == nil {
			t.Fatal("every install step succeeded, no daemon ever answered, and the ensure reported success")
		}
		if !errors.Is(err, macagent.ErrNotAnswering) {
			t.Errorf("the failure does not say the socket never answered: %v", err)
		}
	})
}

// TestAnAlreadyRunningDaemonIsNotReinstalled pins the order the ensure works in. Installing first and
// asking afterwards would reinstall on every bring-up — and on a machine that cannot elevate it would
// refuse one that was already working, which is failing closed on the wrong fact.
func TestAnAlreadyRunningDaemonIsNotReinstalled(t *testing.T) {
	ran := &recorder{}
	ensure := ensureFixture(t, ran)
	ensure.Elevation = macagent.ElevationNone
	startStubDaemon(t, ensure.Probe.SocketPath, "")

	status, err := ensure.apply(context.Background())
	if err != nil {
		t.Fatalf("a machine whose daemon is already answering was refused: %v", err)
	}
	if status.Installed {
		t.Error("a daemon that was already running was reinstalled")
	}
	for _, forbidden := range []string{"/usr/bin/install", "/usr/sbin/dseditgroup", "/bin/launchctl"} {
		if ran.sawBinary(forbidden) {
			t.Errorf("a probe that answered still ran %s: %v", forbidden, ran.commands)
		}
	}
}

// TestAVersionMismatchIsAnUpgradeOrAWarningAndNeverARefusal.
//
// The two versions come from two different places and only one is authority: the daemon's off the
// SOCKET, the candidate's from running the build with -version. They diverge in exactly the case worth
// catching — a copy that landed and a job that was never restarted leave a NEW FILE and an OLD DAEMON.
//
// A mismatch is never a refusal. A machine whose daemon works and whose stamp drifted can still isolate
// a session, and taking it out of the pool for a stamp would be a refusal for a cause that has nothing
// to do with isolation.
func TestAVersionMismatchIsAnUpgradeOrAWarningAndNeverARefusal(t *testing.T) {
	t.Run("without elevation it is left alone with a warning", func(t *testing.T) {
		ran := &recorder{}
		ensure := ensureFixture(t, ran)
		ensure.Elevation = macagent.ElevationNone
		startStubDaemon(t, ensure.Probe.SocketPath, "0.9.0")
		ran.version = "2.0.0"

		status, err := ensure.apply(context.Background())
		if err != nil {
			t.Fatalf("a working daemon was refused for carrying a different stamp: %v", err)
		}
		if len(status.Warnings) != 1 || !strings.Contains(status.Warnings[0], "0.9.0") || !strings.Contains(status.Warnings[0], "2.0.0") {
			t.Errorf("the mismatch was not reported with both stamps: %v", status.Warnings)
		}
		if ran.sawBinary("/bin/launchctl") {
			t.Error("a machine that cannot elevate tried to upgrade anyway")
		}
	})

	t.Run("with elevation it is upgraded", func(t *testing.T) {
		ran := &recorder{}
		ensure := ensureFixture(t, ran)
		ensure.Elevation = macagent.ElevationRoot
		stub := startStubDaemon(t, ensure.Probe.SocketPath, "0.9.0")
		ran.version = "2.0.0"
		// The upgrade replaces what is answering, which is the only thing that decides anything.
		ran.onBootstrap = func() { stub.stamp = "2.0.0" }

		status, err := ensure.apply(context.Background())
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		if !status.Installed || status.Health.Version != "2.0.0" {
			t.Errorf("the daemon was not upgraded: installed=%v version=%q", status.Installed, status.Health.Version)
		}
	})
}

// ensureFixture builds the ensure production builds, with the SHIPPED probe policy, the SHIPPED plist
// bytes and a socket that is genuinely absent. Only the clock and the exec boundary are substituted.
func ensureFixture(t *testing.T, ran *recorder) ensureAgentd {
	t.Helper()
	dir, err := os.MkdirTemp("", "agentd")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	source := filepath.Join(dir, "palai-agentd")
	if err := os.WriteFile(source, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	staging := filepath.Join(dir, "staging")
	if err := os.Mkdir(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	// The bytes an installed machine gets, read from the same embed the CLI reads. A fixture that made
	// its own plist would prove an install works against XML no machine ever receives.
	plist, err := macosdeploy.LaunchDaemonPlist()
	if err != nil {
		t.Fatal(err)
	}
	probe := macagent.NewProber(filepath.Join(dir, "s.sock"))
	probe.Sleep = func(time.Duration) {}
	return ensureAgentd{
		Probe:       probe,
		Run:         ran.run,
		Source:      source,
		Plist:       plist,
		SelfCommand: "sudo /opt/palai/bin/palai agentd install",
		TempDir:     staging,
	}
}

// recorder is the exec boundary: it records every argv and never runs one.
type recorder struct {
	commands [][]string
	// fail names binaries whose invocation returns an error, which is how `sudo -n` is made to refuse.
	fail map[string]bool
	// version is what the candidate binary answers to -version.
	version string
	// onBootstrap fires when launchd is asked to load the job — the moment a real machine's socket
	// starts answering.
	onBootstrap func()
}

func (r *recorder) run(_ context.Context, name string, args ...string) (string, error) {
	r.commands = append(r.commands, append([]string{name}, args...))
	if r.fail[name] {
		return "", errors.New("refused")
	}
	if len(args) == 1 && args[0] == "-version" {
		return r.version + "\n", nil
	}
	if r.onBootstrap != nil && len(args) > 0 && args[0] == "bootstrap" {
		r.onBootstrap()
	}
	// `dseditgroup -o read` on a group that does not exist yet: the install creates it, which is the
	// idempotent branch a re-run and every upgrade take.
	if strings.HasSuffix(name, "dseditgroup") && len(args) >= 2 && args[1] == "read" {
		return "", errors.New("eDSRecordNotFound")
	}
	return "", nil
}

func (r *recorder) sawBinary(name string) bool {
	for _, c := range r.commands {
		for _, tok := range c {
			if tok == name {
				return true
			}
		}
	}
	return false
}

func (r *recorder) sawArg(arg string) bool { return r.sawBinary(arg) }

// stubDaemon answers `list` and `version` on a real unix socket, with a stamp the test can change to
// stand in for an upgrade replacing what is running.
type stubDaemon struct{ stamp string }

func startStubDaemon(t *testing.T, sock, stamp string) *stubDaemon {
	t.Helper()
	d := &stubDaemon{stamp: stamp}
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("binding the stand-in daemon on %s: %v", sock, err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, macagent.MaxRequestBytes)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				if strings.HasPrefix(string(buf[:n]), "version") {
					if d.stamp == "" {
						_, _ = conn.Write([]byte("err unknown_verb no such verb\n"))
						return
					}
					_, _ = conn.Write([]byte("ok version " + d.stamp + "\n"))
					return
				}
				_, _ = conn.Write([]byte("ok list\n"))
			}()
		}
	}()
	return d
}
