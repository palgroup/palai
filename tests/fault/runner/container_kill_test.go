//go:build fault

// Package runner holds the fault-injection proof for the OCI engine supervisor. It
// runs only under `make test-fault TEST=runner`, which cross-builds the fixture
// engine into a digest-pinned `FROM scratch` image and exports its immutable ID as
// PALAI_RUNNER_ENGINE_IMAGE_ID. The build tag keeps these Docker-bound kill/JSONL
// tests out of the credential-free, Docker-free unit tier.
//
// Every test drives packages/runner's Supervisor over the real Moby driver against a
// real container: an engine that hangs is force-killed and classified terminal
// (never a false success), and an engine that emits malformed or oversized stdout
// fails the attempt safely. Each path force-removes the container, so a leaked
// allocation is a test failure, not a silent resource.
package runner

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
)

// TestTimedOutEngineIsKilledAndClassifiedTerminal proves a hung engine is force-killed
// at the wall-time bound and reported as ErrEngineTimeout — a terminal/lost outcome,
// never a false success — with the container gone afterward.
func TestTimedOutEngineIsKilledAndClassifiedTerminal(t *testing.T) {
	supervisor := newSupervisor(t)
	request := fixtureRequest(engineDigest(t), "hang")
	request.Limits.WallTimeMS = 200

	started := time.Now()
	result, err := supervisor.Run(context.Background(), request)
	if !errors.Is(err, runner.ErrEngineTimeout) {
		t.Fatalf("timeout error = %v, want ErrEngineTimeout", err)
	}
	if len(result.Frames) != 0 {
		t.Fatalf("a killed engine reported %d frames; a timeout must not surface a completion", len(result.Frames))
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("forced timeout took %s, want <= 5s", elapsed)
	}
	assertContainerGone(t, result.ContainerID)
}

// TestMalformedEngineOutputFailsSafely proves non-JSONL stdout fails the attempt as an
// invalid-output protocol error, with the container destroyed and no partial frames.
func TestMalformedEngineOutputFailsSafely(t *testing.T) {
	supervisor := newSupervisor(t)
	result, err := supervisor.Run(context.Background(), fixtureRequest(engineDigest(t), "malformed"))
	if !errors.Is(err, runner.ErrInvalidEngineOutput) {
		t.Fatalf("malformed stdout error = %v, want ErrInvalidEngineOutput", err)
	}
	if len(result.Frames) != 0 {
		t.Fatalf("malformed output surfaced %d frames, want 0", len(result.Frames))
	}
	assertContainerGone(t, result.ContainerID)
}

// TestOversizedStdoutIsRejected proves stdout exceeding its configured bound fails the
// attempt as a stdout-limit error rather than truncating into a "valid" frame.
func TestOversizedStdoutIsRejected(t *testing.T) {
	supervisor := newSupervisor(t)
	request := fixtureRequest(engineDigest(t), "oversized")
	request.Limits.MaxStdoutBytes = 1024
	request.Limits.MaxFrameBytes = 512
	result, err := supervisor.Run(context.Background(), request)
	if !errors.Is(err, runner.ErrStdoutLimit) {
		t.Fatalf("oversized stdout error = %v, want ErrStdoutLimit", err)
	}
	assertContainerGone(t, result.ContainerID)
}

func engineDigest(t *testing.T) string {
	t.Helper()
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if digest == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-fault TEST=runner")
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
		RunID:       "run_faultfixture",
		AttemptID:   "att_faultfixture",
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
			return // inspect fails: the allocation is unfindable by ID
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still exists after supervisor returned", containerID[:12])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
