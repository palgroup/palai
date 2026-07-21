//go:build fault

// Package recovery holds the E10 kill-matrix fault proofs that need a REAL engine container:
// the tail-frame drain (REC-001) and an external container kill (ENG-005 streaming half). It
// runs only under `make test-fault CASE=recovery`, which cross-builds the same digest-pinned
// fixture engine the runner suite uses and exports its immutable ID as
// PALAI_RUNNER_ENGINE_IMAGE_ID. The build tag keeps these Docker-bound tests out of the
// credential-free, Docker-free unit tier.
//
// The helpers below mirror tests/fault/runner's — a separate build target cannot import another
// package's test helpers, so the small streaming-supervisor scaffolding is duplicated here.
package recovery

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/packages/runner"
)

func engineDigest(t *testing.T) string {
	t.Helper()
	digest := os.Getenv("PALAI_RUNNER_ENGINE_IMAGE_ID")
	if digest == "" {
		t.Skip("PALAI_RUNNER_ENGINE_IMAGE_ID is required; run make test-fault CASE=recovery")
	}
	return digest
}

func newStreamSupervisor(t *testing.T) *runner.StreamSupervisor {
	t.Helper()
	driver, err := oci.NewDockerInteractiveDriver()
	if err != nil {
		t.Fatalf("create Docker interactive driver: %v", err)
	}
	t.Cleanup(func() {
		if closer, ok := driver.(interface{ Close() error }); ok {
			if err := closer.Close(); err != nil {
				t.Errorf("close interactive driver: %v", err)
			}
		}
	})
	return runner.NewStreamSupervisor(driver)
}

func fixtureRequest(digest, mode string) runner.EngineRequest {
	return runner.EngineRequest{
		ImageDigest: digest,
		RunID:       "run_recoveryfixture",
		AttemptID:   "att_recoveryfixture",
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
		return // an attempt that failed before the container was created has nothing to assert
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

// removeSandboxContainers force-removes every live engine sandbox container (labelled
// io.palai.sandbox=engine). The fault suite runs one attempt at a time, so this is the
// external `docker rm -f` kill of the engine under test.
func removeSandboxContainers(t *testing.T) {
	t.Helper()
	ids, err := exec.Command("docker", "ps", "-q", "--filter", "label=io.palai.sandbox=engine").Output()
	if err != nil {
		t.Fatalf("list sandbox containers: %v", err)
	}
	matched := strings.Fields(string(ids))
	if len(matched) == 0 {
		// A drifted label would match nothing and silently degrade the container-kill into a
		// 30s wall-time pass — a false proof. Fail loudly instead.
		t.Fatal("no engine sandbox container (label io.palai.sandbox=engine) matched to kill; the container-kill would degrade to a wall-time pass")
	}
	for _, id := range matched {
		_ = exec.Command("docker", "rm", "-f", id).Run()
	}
}
