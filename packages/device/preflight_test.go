package device

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestAMachineWithNowhereToWriteClaimsNOMode is the rule the whole preflight exists for. Until 2026-08-06
// `user` was claimed on darwin unconditionally, so a Mac that could not create its workspace root enrolled,
// became ready capacity, and failed at the first file write of every session placed on it — after the
// placement and after the lease, on a machine the panel showed as healthy.
func TestAMachineWithNowhereToWriteClaimsNOMode(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		// Both probes say YES, so the only thing deciding the answer is the workspace verdict — otherwise
		// this would pass for a machine that simply had no daemon.
		if got := Measure(goos, "arm64", "1.0.0", true, true, false); len(got.IsolationModes) != 0 {
			t.Errorf("a %s machine with an unusable workspace claims %v — a pool with NO requirement admits a "+
				"machine that declares nothing, so it would take sessions it cannot serve", goos, got.IsolationModes)
		}
		if got := Measure(goos, "arm64", "1.0.0", true, true, true); len(got.IsolationModes) == 0 {
			t.Errorf("a %s machine with a usable workspace and both daemons claims nothing — the gate is "+
				"refusing everything and the test above would pass vacuously", goos)
		}
	}
}

// TestPreflightAcceptsADirectoryItHadToCreate: a first enrolment on a fresh machine has no workspace root
// yet, so a preflight that only accepted an existing directory would refuse every new machine.
func TestPreflightAcceptsADirectoryItHadToCreate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspaces", "nested")
	if err := PreflightWorkspaceRoot(root); err != nil {
		t.Fatalf("PreflightWorkspaceRoot(%s) = %v, want nil — a fresh machine has no workspace root yet", root, err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatalf("the preflight passed without creating the root: %v", err)
	}
	// It leaves nothing behind: a probe file that survived would be one more thing a later sweep has to
	// reason about, and it would sit inside the directory sessions are allocated under.
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("the preflight left %d entries in the workspace root, want 0", len(entries))
	}
}

// TestPreflightRefusesARootItCannotWriteInto drives the case the field actually produces: the directory is
// there and the agent cannot put a file in it.
//
// ‼️ ROOT SKIPS IT, and the skip is the honest half. A process running as uid 0 writes into a 0500
// directory, so this test would pass for the wrong reason rather than fail — and a permission fixture that
// cannot bind is worse than no fixture, because it reports green.
func TestPreflightRefusesARootItCannotWriteInto(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0500 directory does not refuse this process, so the case cannot be built here")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX mode bits")
	}
	root := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) }) // so t.TempDir's cleanup can remove it

	err := PreflightWorkspaceRoot(root)
	if err == nil {
		t.Fatal("PreflightWorkspaceRoot accepted a root it cannot write into — the machine would enrol and " +
			"fail at the first file write of every session")
	}
	// The reason names the PATH, because the operator reading a service log is the one who can fix it.
	if !strings.Contains(err.Error(), root) {
		t.Fatalf("the refusal %q does not name the directory an operator has to fix", err)
	}
}

// TestAnEmptyWorkspaceRootIsRefusedRatherThanCreated guards the one input that would otherwise be silently
// turned into a relative path: MkdirAll("") fails, but by an error about the empty string rather than about
// a machine that was never told where to put a session.
func TestAnEmptyWorkspaceRootIsRefusedRatherThanCreated(t *testing.T) {
	if err := PreflightWorkspaceRoot(""); err == nil {
		t.Fatal("an empty workspace root passed the preflight")
	}
}
