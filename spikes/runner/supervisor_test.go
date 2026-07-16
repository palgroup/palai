package runner

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"testing"
	"time"
)

func TestSupervisorRejectsMutableImageReferenceBeforeCreate(t *testing.T) {
	request := fixtureEngineRequest("palai/spike-engine:latest", "valid")
	if err := request.Validate(); !errors.Is(err, ErrMutableImage) {
		t.Fatalf("validate mutable image: got %v, want ErrMutableImage", err)
	}
}

func TestSupervisorAcceptsValidBoundedJSONL(t *testing.T) {
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		result, err := supervisor.Run(context.Background(), fixtureEngineRequest(imageID, "valid"))
		if err != nil {
			t.Fatalf("run valid fixture: %v", err)
		}
		if len(result.Frames) != 1 {
			t.Fatalf("frame count = %d, want 1", len(result.Frames))
		}
		frame := result.Frames[0]
		if frame.Protocol != EngineProtocolV1 || frame.Type != "run.completed" || frame.Sequence != 1 {
			t.Fatalf("unexpected engine frame: %#v", frame)
		}
		if result.ImageID != imageID || result.ExitCode != 0 || result.StdoutBytes <= 0 {
			t.Fatalf("unexpected result identity/exit/output: %#v", result)
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func TestSupervisorRejectsMalformedStdout(t *testing.T) {
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		result, err := supervisor.Run(context.Background(), fixtureEngineRequest(imageID, "malformed"))
		if !errors.Is(err, ErrInvalidEngineOutput) {
			t.Fatalf("malformed stdout error = %v, want ErrInvalidEngineOutput", err)
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func TestSupervisorRejectsOversizedStdout(t *testing.T) {
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		request := fixtureEngineRequest(imageID, "oversized")
		request.Limits.MaxStdoutBytes = 1_024
		request.Limits.MaxFrameBytes = 512
		result, err := supervisor.Run(context.Background(), request)
		if !errors.Is(err, ErrStdoutLimit) {
			t.Fatalf("oversized stdout error = %v, want ErrStdoutLimit", err)
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func TestSupervisorCapsStderrSeparately(t *testing.T) {
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		request := fixtureEngineRequest(imageID, "stderr")
		request.Limits.MaxStderrBytes = 128
		result, err := supervisor.Run(context.Background(), request)
		if err != nil {
			t.Fatalf("run stderr fixture: %v", err)
		}
		if !result.StderrTruncated || len(result.Stderr) != int(request.Limits.MaxStderrBytes) {
			t.Fatalf("stderr bound = %d truncated=%v, want %d/true", len(result.Stderr), result.StderrTruncated, request.Limits.MaxStderrBytes)
		}
		if len(result.Frames) != 1 {
			t.Fatalf("stderr affected protocol frames: got %d", len(result.Frames))
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func TestSupervisorDoesNotExposeCredentialsOrDockerSocket(t *testing.T) {
	for _, name := range []string{
		"OPENAI_API_KEY",
		"ANTROPHIC_API_KEY",
		"ANTHROPIC_API_KEY",
		"DATABASE_URL",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"PALAI_RUNNER_PRIVATE_KEY",
	} {
		t.Setenv(name, "test-only-must-not-enter-engine")
	}
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		result, err := supervisor.Run(context.Background(), fixtureEngineRequest(imageID, "inspect"))
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
		if err := json.Unmarshal(result.Frames[0].Data, &inspection); err != nil {
			t.Fatalf("decode inspection frame: %v", err)
		}
		if len(inspection.ForbiddenEnvironment) != 0 || inspection.DockerSocketPresent || inspection.RunnerKeyPresent {
			t.Fatalf("engine received forbidden authority: %#v", inspection)
		}
		expectedEnvironment := []string{"HOME", "HOSTNAME", "PALAI_ATTEMPT_ID", "PALAI_ENGINE_MODE", "PALAI_RUN_ID", "PATH"}
		if !slices.Equal(inspection.EnvironmentNames, expectedEnvironment) {
			t.Fatalf("engine environment names = %v, want exact allowlist %v", inspection.EnvironmentNames, expectedEnvironment)
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func TestSupervisorKillsTimedOutContainer(t *testing.T) {
	withSupervisorIterations(t, func(t *testing.T, supervisor *Supervisor, imageID string) {
		request := fixtureEngineRequest(imageID, "hang")
		request.Limits.WallTimeMS = 200
		started := time.Now()
		result, err := supervisor.Run(context.Background(), request)
		if !errors.Is(err, ErrEngineTimeout) {
			t.Fatalf("timeout error = %v, want ErrEngineTimeout", err)
		}
		if elapsed := time.Since(started); elapsed > 5*time.Second {
			t.Fatalf("forced timeout took %s, want <= 5s", elapsed)
		}
		assertContainerAbsent(t, result.ContainerID)
	})
}

func withSupervisorIterations(t *testing.T, test func(*testing.T, *Supervisor, string)) {
	t.Helper()
	imageID := os.Getenv("PALAI_SPIKE_RUNNER_IMAGE_ID")
	if imageID == "" {
		t.Skip("PALAI_SPIKE_RUNNER_IMAGE_ID is required for Docker supervision tests")
	}
	iterations := 1
	if raw := os.Getenv("PALAI_SPIKE_RUNNER_ITERATIONS"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			t.Fatalf("invalid PALAI_SPIKE_RUNNER_ITERATIONS %q", raw)
		}
		iterations = parsed
	}
	for iteration := 1; iteration <= iterations; iteration++ {
		t.Run(strconv.Itoa(iteration), func(t *testing.T) {
			supervisor, err := NewSupervisor(SupervisorConfig{OperationTimeout: 5 * time.Second})
			if err != nil {
				t.Fatalf("create supervisor: %v", err)
			}
			t.Cleanup(func() {
				if err := supervisor.Close(); err != nil {
					t.Errorf("close supervisor: %v", err)
				}
			})
			test(t, supervisor, imageID)
		})
	}
}

func fixtureEngineRequest(imageID, mode string) EngineRequest {
	return EngineRequest{
		ImageID:   imageID,
		RunID:     "run-supervisor-fixture",
		AttemptID: "attempt-supervisor-fixture",
		Mode:      mode,
		Limits: LeaseLimits{
			WallTimeMS:      3_000,
			MaxStdoutBytes:  32 * 1024,
			MaxStderrBytes:  4 * 1024,
			MaxFrameBytes:   8 * 1024,
			MaxMemoryBytes:  64 * 1024 * 1024,
			MaxProcessCount: 16,
		},
	}
}

func assertContainerAbsent(t *testing.T, containerID string) {
	t.Helper()
	if containerID == "" {
		t.Fatal("supervisor did not return created container ID")
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		command := exec.Command("docker", "container", "inspect", containerID)
		if err := command.Run(); err != nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("container %s still exists after supervisor returned", containerID[:12])
		}
		time.Sleep(50 * time.Millisecond)
	}
}
