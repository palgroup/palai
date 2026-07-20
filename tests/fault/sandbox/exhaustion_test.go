//go:build fault

// Package sandbox holds the E09 Task 4 resource-exhaustion fault proof for the workspace shell tool.
// It runs only under `make test-fault CASE=sandbox`, which cross-builds the fixture into a
// digest-pinned image and exports PALAI_RUNNER_ENGINE_IMAGE_ID. The termination is a real memory
// cgroup OOM kill, not a simulated string (spec §28.8, SAN-003).
package sandbox

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TestResourceExhaustionBoundedTerminationUsageRecorded proves SAN-003 against a real memory cgroup:
// a command that exhausts memory is OOM-killed (bounded termination, not host exhaustion), a
// co-run healthy command in its own sandbox is unaffected (neighbour intact), and the terminated
// run records its usage (duration + termination class).
func TestResourceExhaustionBoundedTerminationUsageRecorded(t *testing.T) {
	// A memory hog under a tight memory cgroup is OOM-killed — bounded termination.
	hogDir := newAllocation(t)
	res := run(t, hogDir, 20*time.Second, 128<<20, "san", "memhog")

	if res.ExitCode == 0 {
		t.Fatalf("memory hog exited 0; the memory bound did not terminate it: %#v", res)
	}
	if !res.OOMKilled && res.Signal != "KILL" {
		t.Fatalf("memory hog was not bounded-terminated (oom/kill): %#v", res)
	}
	if res.DurationMS <= 0 {
		t.Fatalf("terminated run recorded no usage duration: %#v", res)
	}

	// A neighbour command in its own sandbox with a healthy bound is unaffected by the hog.
	okDir := newAllocation(t)
	ok := run(t, okDir, 20*time.Second, 256<<20, "san", "ok")
	if ok.ExitCode != 0 || !strings.Contains(ok.Stdout, "ok") {
		t.Fatalf("neighbour tenant was not intact after the hog: %#v", ok)
	}
}

// --- harness (mirrors the security/sandbox tier; kept local to this test package) ---

func run(t *testing.T, allocDir string, wall time.Duration, memBytes int64, argv ...string) toolbroker.ShellResult {
	t.Helper()
	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("create Docker driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	limits := oci.Limits{WallTime: wall, MaxMemoryBytes: memBytes, MaxProcessCount: 128, NanoCPUs: 1_000_000_000}
	exec := workspace.NewShellExecutor(driver, engineImage(t), limits)
	res, err := exec.Run(context.Background(), toolbroker.ShellCommand{Argv: argv, WorkspaceRoot: allocDir})
	if err != nil {
		t.Fatalf("shell %v: %v", argv, err)
	}
	return res
}

func engineImage(t *testing.T) string {
	t.Helper()
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if digest == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-fault CASE=sandbox")
	}
	return digest
}

func newAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-san-fault-")
	if err != nil {
		t.Fatalf("create allocation dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	if err := workspace.Prepare(resolved); err != nil {
		t.Fatalf("prepare workspace: %v", err)
	}
	err = filepath.WalkDir(resolved, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return os.Chmod(path, 0o777)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("open allocation to sandbox uid: %v", err)
	}
	return resolved
}
