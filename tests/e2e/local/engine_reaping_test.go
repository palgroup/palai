//go:build e2e

package local

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// reapTestImage is a locally-present pinned image used only to give the decoy engine
// containers a valid reference; `docker create` never starts them.
const reapTestImage = "postgres@sha256:17e67d7b9890c99b055ba1e0d5c5be4ec27c9d3a72bda32db24a5e5d8a85af0c"

// TestLocalDownReapsThisProjectEngineContainers proves H4: a mid-run-killed engine sandbox —
// a container the runner launched through the Docker socket, not a compose service — is
// force-removed by `palai local down`, filtered to THIS stack's compose project so a
// concurrent stack's engine survives. Without the sweep the engine leaks: `compose down
// --remove-orphans` never sees a container that carries no compose label.
func TestLocalDownReapsThisProjectEngineContainers(t *testing.T) {
	s := newStack(t)
	s.run("init")
	project := s.config().Project

	// A labeled orphan engine for THIS project (simulating a mid-run kill), and a decoy for a
	// DIFFERENT project that must survive (concurrent-stack safety).
	mine := "palai-h4-mine-" + randSuffix(t)
	other := "palai-h4-other-" + randSuffix(t)
	createEngineContainer(t, mine, project)
	createEngineContainer(t, other, project+"-concurrent")
	// reset --confirm (stack teardown) filters to this project, so the decoy is this test's
	// to remove.
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", other).Run() })

	s.run("local", "down")

	if containerExists(t, mine) {
		t.Fatalf("engine container %s for this project survived `local down` (not reaped)", mine)
	}
	if !containerExists(t, other) {
		t.Fatalf("engine container %s for a different project was reaped (concurrent-stack unsafe)", other)
	}
}

// createEngineContainer creates a stopped container carrying the engine sandbox label and a
// compose-project label, mirroring what the runner tags a live engine with.
func createEngineContainer(t *testing.T, name, project string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "create", "--name", name,
		"--label", "io.palai.sandbox=engine",
		"--label", "io.palai.project="+project,
		reapTestImage).CombinedOutput()
	if err != nil {
		t.Fatalf("create engine container %s: %v\n%s", name, err, out)
	}
}

// containerExists reports whether a container with the exact name is present (any state).
func containerExists(t *testing.T, name string) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", "ps", "-aq", "--filter", "name=^/"+name+"$").Output()
	if err != nil {
		t.Fatalf("list container %s: %v", name, err)
	}
	return strings.TrimSpace(string(out)) != ""
}

func randSuffix(t *testing.T) string {
	t.Helper()
	var raw [4]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw[:])
}
