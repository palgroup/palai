//go:build security

// Package runner holds the isolation proof for the OCI engine supervisor. It runs
// only under `make test-security TEST=runner`, which cross-builds the fixture engine
// into a digest-pinned image and exports PALAI_RUNNER_ENGINE_IMAGE_ID. The build tag
// keeps these Docker-bound isolation tests out of the Docker-free unit tier.
//
// Every test drives packages/runner's Supervisor over the real Moby driver and proves
// the untrusted engine receives only an exact environment allowlist — no provider,
// database, object-store, or runner credential, and no Docker socket or runner key —
// that stderr is redacted and bounded independently of stdout, and that the container
// is unfindable by allocation ID once the attempt is destroyed.
package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"slices"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
)

// TestEngineReceivesNoCredentialsOrDockerSocket seeds every forbidden credential into
// the host environment, then proves none reaches the sandboxed engine, that neither
// the Docker socket nor the runner key is mounted, and that the engine sees exactly
// the intended environment allowlist.
func TestEngineReceivesNoCredentialsOrDockerSocket(t *testing.T) {
	for _, name := range []string{
		"OPENAI_API_KEY", "ANTROPHIC_API_KEY", "ANTHROPIC_API_KEY", "DATABASE_URL",
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "PALAI_RUNNER_PRIVATE_KEY",
	} {
		t.Setenv(name, "test-only-must-not-enter-engine")
	}
	supervisor := newSupervisor(t)
	result, err := supervisor.Run(context.Background(), fixtureRequest(engineDigest(t), "inspect"))
	if err != nil {
		t.Fatalf("run inspection fixture: %v", err)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("inspection frame count = %d, want 1", len(result.Frames))
	}

	var inspection struct {
		ForbiddenEnvironment []string `json:"forbidden_environment"`
		EnvironmentNames     []string `json:"environment_names"`
		DockerSocketPresent  bool     `json:"docker_socket_present"`
		RunnerKeyPresent     bool     `json:"runner_key_present"`
	}
	data, err := json.Marshal(result.Frames[0].Data)
	if err != nil {
		t.Fatalf("marshal inspection data: %v", err)
	}
	if err := json.Unmarshal(data, &inspection); err != nil {
		t.Fatalf("decode inspection frame: %v", err)
	}
	if len(inspection.ForbiddenEnvironment) != 0 || inspection.DockerSocketPresent || inspection.RunnerKeyPresent {
		t.Fatalf("engine received forbidden authority: %#v", inspection)
	}
	expected := []string{"HOME", "HOSTNAME", "PALAI_ATTEMPT_ID", "PALAI_ENGINE_MODE", "PALAI_RUN_ID", "PATH"}
	if !slices.Equal(inspection.EnvironmentNames, expected) {
		t.Fatalf("engine environment = %v, want exact allowlist %v", inspection.EnvironmentNames, expected)
	}
	assertContainerGone(t, result.ContainerID)
}

// TestStderrIsBoundedSeparatelyFromStdout proves stderr is captured into its own
// bounded, truncated buffer without corrupting the strict-JSONL stdout stream.
func TestStderrIsBoundedSeparatelyFromStdout(t *testing.T) {
	supervisor := newSupervisor(t)
	request := fixtureRequest(engineDigest(t), "stderr")
	request.Limits.MaxStderrBytes = 128
	result, err := supervisor.Run(context.Background(), request)
	if err != nil {
		t.Fatalf("run stderr fixture: %v", err)
	}
	if !result.StderrTruncated || int64(len(result.Stderr)) != request.Limits.MaxStderrBytes {
		t.Fatalf("stderr bound = %d truncated=%v, want %d/true", len(result.Stderr), result.StderrTruncated, request.Limits.MaxStderrBytes)
	}
	if len(result.Frames) != 1 {
		t.Fatalf("stderr load corrupted stdout: %d frames, want 1", len(result.Frames))
	}
	assertContainerGone(t, result.ContainerID)
}

// TestContainerIsUnfindableAfterDestroy proves a completed attempt leaves no container
// findable by its allocation ID.
func TestContainerIsUnfindableAfterDestroy(t *testing.T) {
	supervisor := newSupervisor(t)
	result, err := supervisor.Run(context.Background(), fixtureRequest(engineDigest(t), "valid"))
	if err != nil {
		t.Fatalf("run valid fixture: %v", err)
	}
	if result.ImageID != fixtureRequest(engineDigest(t), "valid").ImageDigest {
		t.Fatalf("result image = %q, want the pinned digest %q", result.ImageID, engineDigest(t))
	}
	assertContainerGone(t, result.ContainerID)
}

func engineDigest(t *testing.T) string {
	t.Helper()
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if digest == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-security TEST=runner")
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
		RunID:       "run_securityfixture",
		AttemptID:   "att_securityfixture",
		Env:         map[string]string{"PALAI_ENGINE_MODE": mode},
		Limits: runner.Limits{
			WallTimeMS:      3000,
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
