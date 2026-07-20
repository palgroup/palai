//go:build security

// Package sandbox holds the real-OCI isolation proofs for the E09 Task 4 workspace shell tool. It
// runs only under `make test-security TEST=sandbox`, which cross-builds the fixture into a
// digest-pinned image and exports PALAI_RUNNER_ENGINE_IMAGE_ID. Every command runs inside a real
// hardened sandbox (spec §28.8): unprivileged uid, no network, cgroup bounds, workspace-mounted,
// process-group-killed on teardown, no runtime socket. It drives the workspace ShellExecutor — the
// tool's sandbox seam — directly; the tool's thin argv/egress wrapper is unit-tested separately.
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

// TestShellToolArgvFormSandboxUserLimitsProcessGroupKill proves the SAN-002 shell isolation: argv is
// passed verbatim (no shell splitting), the command runs as the unprivileged sandbox uid, no
// container-runtime socket is reachable, and a wall-time teardown kills the entire process group so
// no descendant survives.
func TestShellToolArgvFormSandboxUserLimitsProcessGroupKill(t *testing.T) {
	allocDir := newAllocation(t)

	// argv-form: five separate arguments arrive as five, not one shell-split line.
	echo := run(t, allocDir, 10*time.Second, 256<<20, "san", "echo", "alpha", "beta gamma", "delta")
	if echo.Stdout != "alpha\nbeta gamma\ndelta\n" {
		t.Fatalf("argv-form stdout = %q, want each argument on its own line (no shell splitting)", echo.Stdout)
	}

	// The sandbox runs as the unprivileged uid 65532, never root.
	if who := run(t, allocDir, 10*time.Second, 256<<20, "san", "whoami"); !strings.Contains(who.Stdout, "uid=65532") {
		t.Fatalf("whoami = %q, want uid=65532 (unprivileged sandbox user)", who.Stdout)
	}

	// No container-runtime socket is present or dialable.
	sock := run(t, allocDir, 10*time.Second, 256<<20, "san", "socket")
	if !strings.Contains(sock.Stdout, "docker_socket_present=false") || !strings.Contains(sock.Stdout, "dial_ok=false") {
		t.Fatalf("socket probe = %q, want socket absent and undialable", sock.Stdout)
	}

	// Process-group kill: a short wall-time tears the container down mid-sleep, killing the parent
	// and every child before any child reaches its post-sleep marker.
	res := run(t, allocDir, 3*time.Second, 256<<20, "san", "children", "3")
	if !res.TimedOut {
		t.Fatalf("children run TimedOut = %v, want true (wall-time teardown)", res.TimedOut)
	}
	if !fileExists(filepath.Join(allocDir, workspace.ScratchDir, "parent-started")) {
		t.Fatal("parent never started; the process-group test did not exercise a running tree")
	}
	entries, _ := os.ReadDir(filepath.Join(allocDir, workspace.ScratchDir))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "child-") && strings.HasSuffix(e.Name(), "-done") {
			t.Fatalf("child marker %q survived teardown; the process group was not killed", e.Name())
		}
	}
}

// TestShellSecretRedactedInDisplayAndOutput proves secret-shaped tokens in shell output are masked
// before the executor returns them (spec §28.8) — the value the model and any client see is redacted.
func TestShellSecretRedactedInDisplayAndOutput(t *testing.T) {
	allocDir := newAllocation(t)
	res := run(t, allocDir, 10*time.Second, 256<<20, "san", "secret")
	for _, leaked := range []string{"sk-live-SANFIXTURESECRET", "ghp_SANFIXTUREGITHUBTOKEN", "bearer abcdefgh"} {
		if strings.Contains(res.Stdout, leaked) {
			t.Fatalf("shell output leaked a secret %q: %q", leaked, res.Stdout)
		}
	}
	if !strings.Contains(res.Stdout, "***") {
		t.Fatalf("shell output carried no redaction marker: %q", res.Stdout)
	}
}

// TestSandboxDeniesMetadataEgress proves the SAN-004 enforcement half: a command that dials the cloud
// metadata address is denied by the no-network sandbox. The finding half (the tool flagging a
// metadata target from the argv) is unit-tested in the tools package.
func TestSandboxDeniesMetadataEgress(t *testing.T) {
	allocDir := newAllocation(t)
	res := run(t, allocDir, 10*time.Second, 256<<20, "san", "dial", "169.254.169.254")
	if !strings.Contains(res.Stdout, "dial_ok=false") {
		t.Fatalf("metadata dial was not denied by the sandbox: %q", res.Stdout)
	}
}

// --- harness ---

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
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-security TEST=sandbox")
	}
	return digest
}

// newAllocation creates a Docker-shareable, sandbox-writable workspace allocation under /tmp.
func newAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-san-")
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

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
