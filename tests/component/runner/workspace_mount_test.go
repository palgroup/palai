//go:build component

// Package runner holds the real-OCI component proof that a workspace allocation is bind-mounted
// into the engine sandbox (spec §29.9). It runs only under `make test-component TEST=runner`,
// which cross-builds the fixture engine into a digest-pinned image and exports
// PALAI_RUNNER_ENGINE_IMAGE_ID. The build tag keeps this Docker-bound test out of the unit tier.
package runner

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/runner"
)

// TestEngineContainerMountsWorkspaceVolume proves the real mount end to end: a host allocation
// directory is bind-mounted to /workspace in a real, hardened OCI engine container; the engine
// reads a seed file the control plane staged there, and a file the engine writes persists in the
// host allocation after the container is destroyed. This is the load-bearing real-mount proof for
// E09 Task 1 — the model does not touch /workspace yet (that is the file tool, Task 4).
func TestEngineContainerMountsWorkspaceVolume(t *testing.T) {
	allocDir := newAllocation(t)
	if err := workspace.Prepare(allocDir); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	// The control plane stages a fixed seed the engine will read back.
	const seed = "e09-t1-real-mount-seed"
	if err := os.WriteFile(filepath.Join(allocDir, workspace.RepoDir, "seed"), []byte(seed), 0o644); err != nil {
		t.Fatalf("stage seed: %v", err)
	}
	// The sandbox runs as an unprivileged uid (65532); make the allocation dirs traversable and
	// writable by it so the mount, not host ownership, is what the test exercises.
	makeSandboxWritable(t, allocDir)

	supervisor := newSupervisor(t)
	request := fixtureRequest(engineDigest(t), "workspace")
	request.WorkspaceHostPath = allocDir
	result, err := supervisor.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("run workspace fixture: %v", err)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("frame count = %d, want 1", len(result.Frames))
	}

	var probe struct {
		WorkspacePresent bool   `json:"workspace_present"`
		SeedReadable     bool   `json:"seed_readable"`
		SeedContent      string `json:"seed_content"`
		Wrote            bool   `json:"wrote"`
		WriteError       string `json:"write_error"`
	}
	data, err := json.Marshal(result.Frames[0].Data)
	if err != nil {
		t.Fatalf("marshal probe data: %v", err)
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		t.Fatalf("decode probe frame: %v", err)
	}

	// The engine container saw the real mount and the staged seed.
	if !probe.WorkspacePresent || !probe.SeedReadable || probe.SeedContent != seed {
		t.Fatalf("engine did not see the real /workspace mount: %#v", probe)
	}
	if !probe.Wrote {
		t.Fatalf("engine could not write into the allocation: %s", probe.WriteError)
	}

	// The engine's write persists in the host allocation after the container is destroyed.
	assertContainerGone(t, result.ContainerID)
	persisted, err := os.ReadFile(filepath.Join(allocDir, workspace.ScratchDir, "out"))
	if err != nil {
		t.Fatalf("read persisted workspace write: %v", err)
	}
	if string(persisted) != "engine-wrote-this" {
		t.Fatalf("persisted workspace write = %q, want the engine's bytes", persisted)
	}
}

// newAllocation creates a Docker-shareable allocation directory. It is rooted under /tmp (shared
// by Docker Desktop and trivially by a Linux daemon) and symlink-resolved, since the daemon needs
// the real host path for a bind mount.
func newAllocation(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "palai-ws-mount-")
	if err != nil {
		t.Fatalf("create allocation dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	return resolved
}

// makeSandboxWritable opens every directory in the allocation to the unprivileged sandbox uid, so
// the container can traverse and write regardless of host ownership. Files keep their own modes.
func makeSandboxWritable(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
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
}

func engineDigest(t *testing.T) string {
	t.Helper()
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if digest == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-component TEST=runner")
	}
	return digest
}

func newSupervisor(t *testing.T) *runner.Supervisor {
	t.Helper()
	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("create Docker driver: %v", err)
	}
	t.Cleanup(func() {
		if err := driver.Close(); err != nil {
			t.Errorf("close driver: %v", err)
		}
	})
	return runner.NewSupervisor(driver)
}

func fixtureRequest(digest, mode string) runner.EngineRequest {
	return runner.EngineRequest{
		ImageDigest: digest,
		RunID:       "run_workspacefixture",
		AttemptID:   "att_workspacefixture",
		Env:         map[string]string{"PALAI_ENGINE_MODE": mode},
		Limits: runner.Limits{
			WallTimeMS:      5000,
			MaxStdoutBytes:  32 * 1024,
			MaxStderrBytes:  4 * 1024,
			MaxFrameBytes:   8 * 1024,
			MaxMemoryBytes:  64 * 1024 * 1024,
			MaxProcessCount: 16,
		},
	}
}

func assertContainerGone(t *testing.T, containerID string) {
	t.Helper()
	if containerID == "" {
		t.Fatal("supervisor did not return a created container ID")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if err := exec.Command("docker", "container", "inspect", containerID).Run(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still exists after supervisor returned", containerID[:12])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
