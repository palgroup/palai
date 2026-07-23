// E14 T5 — the compose-config invariant behind the split-VM runner host package: the runner is
// OUTBOUND-ONLY (no listen/published port) and the Docker socket reaches ONLY the runner (the
// trusted supervisor), never a workload. This is the static (Docker-free) compose-level assert
// the plan asks for; it complements — does not rebuild — configvalidate.go's edge-surface check
// (host-published ports) and tests/security/runner (the engine gets no socket at runtime).
package compose

import (
	"os"
	"testing"

	"gopkg.in/yaml.v3"
)

// composeService is the slice of a compose service the invariant reads: its published/listen
// ports and its bind mounts. yaml.Node keeps `ports` faithful to how compose merges it.
type composeService struct {
	Ports   yaml.Node `yaml:"ports"`
	Volumes []string  `yaml:"volumes"`
}

type composeDoc struct {
	Services map[string]composeService `yaml:"services"`
}

const dockerSocket = "/var/run/docker.sock"

func loadCompose(t *testing.T, path string) composeDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return doc
}

// mountsDockerSocket reports whether any of the service's bind mounts source the host Docker
// socket (the "<host>:<container>" form compose uses).
func mountsDockerSocket(svc composeService) bool {
	for _, v := range svc.Volumes {
		if len(v) >= len(dockerSocket) && v[:len(dockerSocket)] == dockerSocket {
			return true
		}
	}
	return false
}

// TestRunnerIsOutboundOnlyAndSocketIsRunnerOnly pins, at the compose-config level, the two
// properties the runner host package depends on:
//   - the runner service declares NO ports (it opens no listen port; it dials the gateway out);
//   - the Docker socket is mounted into the runner and NOTHING else, and there is no `engine`
//     compose service — the engine is launched BY the runner through that socket, so it can
//     never receive a compose socket of its own.
func TestRunnerIsOutboundOnlyAndSocketIsRunnerOnly(t *testing.T) {
	base := loadCompose(t, "compose.yaml")

	runner, ok := base.Services["runner"]
	if !ok {
		t.Fatal("compose.yaml has no `runner` service")
	}
	if runner.Ports.Kind != 0 {
		t.Fatalf("runner declares `ports` — it must be outbound-only (no listen port): %v", runner.Ports)
	}
	if !mountsDockerSocket(runner) {
		t.Fatal("runner does not mount the Docker socket — it is the supervisor and needs it")
	}

	// The engine is never a compose service; it is spawned by the runner. If a service named
	// "engine" ever appeared it could be handed a socket, which is exactly what must never happen.
	if _, ok := base.Services["engine"]; ok {
		t.Fatal("compose.yaml has an `engine` service — the engine must be runner-launched, never a compose service with its own socket")
	}

	// Exactly the runner may mount the socket. Any other service mounting it would hand the socket
	// to a component that is not the trusted supervisor.
	for name, svc := range base.Services {
		if name == "runner" {
			continue
		}
		if mountsDockerSocket(svc) {
			t.Fatalf("service %q mounts the Docker socket — only the runner (supervisor) may", name)
		}
	}
}

// TestProductionOverlayKeepsRunnerOutboundOnly proves the T1 production overlay does not
// re-expose the runner or hand any service the Docker socket — the hardened posture keeps the
// base invariant intact (the overlay resets host ports on the published services and adds none).
func TestProductionOverlayKeepsRunnerOutboundOnly(t *testing.T) {
	overlay := loadCompose(t, "production.yml")

	if runner, ok := overlay.Services["runner"]; ok {
		if runner.Ports.Kind != 0 && runner.Ports.Tag != "!reset" && runner.Ports.Tag != "!override" {
			t.Fatalf("production overlay adds `ports` to the runner — it must stay outbound-only: %v", runner.Ports)
		}
	}
	for name, svc := range overlay.Services {
		if mountsDockerSocket(svc) {
			t.Fatalf("production overlay mounts the Docker socket into %q — only the base runner mount is allowed", name)
		}
	}
}
