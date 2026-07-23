// E14 T5 — the compose-config invariant behind the split-VM runner host package: the runner is
// OUTBOUND-ONLY (no listen/published port) and the Docker socket reaches ONLY the runner (the
// trusted supervisor), never a workload. This is the static (Docker-free) compose-level assert
// the plan asks for; it complements — does not rebuild — configvalidate.go's edge-surface check
// (host-published ports) and tests/security/runner (the engine gets no socket at runtime).
package compose

import (
	"fmt"
	"os"
	"strings"
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

func loadComposeDoc(t *testing.T, path string) composeDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return parseComposeDoc(t, raw)
}

func parseComposeDoc(t *testing.T, raw []byte) composeDoc {
	t.Helper()
	var doc composeDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse compose: %v", err)
	}
	return doc
}

// mountsDockerSocket reports whether a service bind-mounts the host Docker socket. It catches the
// socket named ANYWHERE in the mount ("…/docker.sock…", incl. a "${DOCKER_SOCK}:/…/docker.sock"
// form whose destination reveals it) AND a parent run-dir mount (/var/run or /run — which
// CONTAINS the socket). A plain source-prefix check missed both, which is exactly the bypass this
// guards. The source is the part before the first ":" of a "<src>:<dst>[:opts]" bind.
func mountsDockerSocket(svc composeService) bool {
	for _, v := range svc.Volumes {
		if strings.Contains(v, "docker.sock") {
			return true
		}
		src := v
		if i := strings.IndexByte(v, ':'); i >= 0 {
			src = v[:i]
		}
		if src == "/var/run" || src == "/run" || strings.HasPrefix(src, "/var/run/") || strings.HasPrefix(src, "/run/") {
			return true
		}
	}
	return false
}

// publishesPorts reports whether a service's `ports` node carries any actual entries. An empty
// list — including a `!reset []` / `!override []` that CLEARS the base ports — is not a published
// port; a tagged-but-NON-empty list (`ports: !override ["0.0.0.0:9000:9000"]`) IS, and must not
// slip through on the tag alone (the merge tag is irrelevant once the list has content).
func publishesPorts(svc composeService) bool {
	return len(svc.Ports.Content) > 0
}

// checkRunnerOutboundAndSocketRunnerOnly is the base-compose invariant, returning an error so both
// the real file and adversarial synthetic docs can drive it.
func checkRunnerOutboundAndSocketRunnerOnly(doc composeDoc) error {
	runner, ok := doc.Services["runner"]
	if !ok {
		return fmt.Errorf("compose has no `runner` service")
	}
	if publishesPorts(runner) {
		return fmt.Errorf("runner declares ports — it must be outbound-only (no listen port)")
	}
	if !mountsDockerSocket(runner) {
		return fmt.Errorf("runner does not mount the Docker socket — it is the supervisor and needs it")
	}
	// The engine is never a compose service; it is spawned by the runner. A service named "engine"
	// could be handed a socket, which is exactly what must never happen.
	if _, ok := doc.Services["engine"]; ok {
		return fmt.Errorf("compose has an `engine` service — the engine must be runner-launched, never a compose service with its own socket")
	}
	// Exactly the runner may mount the socket.
	for name, svc := range doc.Services {
		if name == "runner" {
			continue
		}
		if mountsDockerSocket(svc) {
			return fmt.Errorf("service %q mounts the Docker socket — only the runner (supervisor) may", name)
		}
	}
	return nil
}

// checkOverlayKeepsRunnerOutbound is the overlay invariant: the overlay must not re-expose the
// runner or hand any service the socket.
func checkOverlayKeepsRunnerOutbound(doc composeDoc) error {
	if runner, ok := doc.Services["runner"]; ok && publishesPorts(runner) {
		return fmt.Errorf("overlay adds ports to the runner — it must stay outbound-only")
	}
	for name, svc := range doc.Services {
		if mountsDockerSocket(svc) {
			return fmt.Errorf("overlay mounts the Docker socket into %q — only the base runner mount is allowed", name)
		}
	}
	return nil
}

func TestRunnerIsOutboundOnlyAndSocketIsRunnerOnly(t *testing.T) {
	if err := checkRunnerOutboundAndSocketRunnerOnly(loadComposeDoc(t, "compose.yaml")); err != nil {
		t.Fatalf("base compose invariant broken: %v", err)
	}
}

func TestProductionOverlayKeepsRunnerOutboundOnly(t *testing.T) {
	if err := checkOverlayKeepsRunnerOutbound(loadComposeDoc(t, "production.yml")); err != nil {
		t.Fatalf("production overlay invariant broken: %v", err)
	}
}

// TestOverlayWithRunnerPortsIsRejected is the RED for MF2: a runner `ports: !override [<port>]`
// (production.yml already uses the !reset/!override idiom, so this is realistic) must be rejected —
// a tag-only check let it through while the runner published a port.
func TestOverlayWithRunnerPortsIsRejected(t *testing.T) {
	malicious := parseComposeDoc(t, []byte(`
services:
  runner:
    ports: !override
      - "0.0.0.0:9000:9000"
`))
	if err := checkOverlayKeepsRunnerOutbound(malicious); err == nil {
		t.Fatal("overlay assert PASSED a runner with a published port under !override — it must fail closed")
	}
}

// TestNonRunnerRunDirMountIsRejected is the RED for the socket-scan NIT: a non-runner service
// mounting the run DIR (/var/run:/var/run, which contains the socket) must be rejected — a plain
// docker.sock-prefix check missed it.
func TestNonRunnerRunDirMountIsRejected(t *testing.T) {
	malicious := parseComposeDoc(t, []byte(`
services:
  postgres:
    volumes:
      - "/var/run:/var/run"
  runner:
    volumes:
      - "/var/run/docker.sock:/var/run/docker.sock"
`))
	if err := checkRunnerOutboundAndSocketRunnerOnly(malicious); err == nil {
		t.Fatal("base assert PASSED a non-runner service mounting the run dir — it hands the socket to a non-supervisor")
	}
}
