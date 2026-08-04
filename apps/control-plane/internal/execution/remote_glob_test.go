package execution_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestGlobSearchesTheMachineThatHoldsTheLease is the remote half of the Glob op, and it is the half
// that can be wrong in a way no local test would notice: a control plane that walked its OWN
// filesystem here would still return paths, and would still look correct on a developer machine
// where the two happen to be the same directory. This test makes them different on purpose — the
// machine's disk carries the files, this process's working directory does not — so an implementation
// that searched locally comes back empty.
func TestGlobSearchesTheMachineThatHoldsTheLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	for i, rel := range []string{"top.go", "notes.md", "src/a.go", "src/deep/b.go"} {
		abs := filepath.Join(disk.alloc, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			t.Fatalf("mkdir for %s: %v", rel, err)
		}
		if err := os.WriteFile(abs, []byte("x"), 0o644); err != nil {
			t.Fatalf("write %s: %v", rel, err)
		}
		when := time.Now().Add(time.Duration(i-10) * time.Minute)
		if err := os.Chtimes(abs, when, when); err != nil {
			t.Fatalf("chtimes %s: %v", rel, err)
		}
	}

	machineExec := &recordingExecutor{result: toolbroker.ShellResult{ExitCode: 0}}
	ch := remoteShellFixture(t, ctx, "t5-glob-token", "run_t5glob", servingMachine(disk, machineExec))
	ws := remoteWorkspaceOn(t, ch, disk.alloc)

	paths, truncated, err := ws.Glob(ctx, "**/*.go", 0)
	if err != nil {
		t.Fatalf("remote glob: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("remote glob matched %v, want the machine's three .go files", paths)
	}
	if truncated {
		t.Error("an uncapped remote glob reported truncation")
	}
	// Newest-first survives the wire: b.go was written last of the three .go files.
	if paths[0] != "src/deep/b.go" {
		t.Errorf("newest match is %q, want src/deep/b.go — the ordering did not survive the round trip", paths[0])
	}

	capped, truncatedCap, err := ws.Glob(ctx, "**/*", 2)
	if err != nil {
		t.Fatalf("remote glob with a cap: %v", err)
	}
	if len(capped) != 2 || !truncatedCap {
		t.Errorf("capped remote glob returned %v (truncated=%v), want 2 paths and truncated=true", capped, truncatedCap)
	}
}

// TestARemoteGlobRefusalKeepsItsMeaningAcrossTheWire — an escaping pattern is refused on the machine,
// and the refusal has to arrive as an error rather than as an empty result. An empty list reads as
// "there are no such files", which sends a caller looking for the wrong cause.
func TestARemoteGlobRefusalKeepsItsMeaningAcrossTheWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	disk := newMachineDisk(t)
	machineExec := &recordingExecutor{result: toolbroker.ShellResult{ExitCode: 0}}
	ch := remoteShellFixture(t, ctx, "t5-globref-token", "run_t5globref", servingMachine(disk, machineExec))
	ws := remoteWorkspaceOn(t, ch, disk.alloc)

	if _, _, err := ws.Glob(ctx, "../*", 0); err == nil {
		t.Error("an escaping pattern came back with no error from the machine")
	}
}
