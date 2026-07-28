package compose

import (
	"os"
	"strings"
	"testing"
)

// The native-control-plane overlay is what a Mac deployment IS (E22 T1): the control plane runs on
// the host so it can reach the host's toolchain, and Postgres, the object store and the runner stay
// in Docker. Three properties make that combination work, and each fails SILENTLY if it rots — a
// dangling depends_on, a certificate name that no longer resolves, a workspace path the Docker
// daemon cannot find. So they are pinned here.
//
// This guard is Docker-free and rides `make verify`. It does NOT claim the overlay brings a stack
// up; that is an operator leg (docs/operations/palai-on-a-mac.md §4). It claims the three things a
// reader would otherwise have to re-derive from two files.
const nativeOverlay = "native-control-plane.yml"

func readOverlay(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(nativeOverlay)
	if err != nil {
		t.Fatalf("read %s: %v", nativeOverlay, err)
	}
	return string(b)
}

// TestNativeOverlayDoesNotStartTheContainerControlPlane pins the point of the overlay. A profile
// rather than a removal: the runner's depends_on would dangle against a deleted service, and an
// operator debugging a native stack wants the container one an A/B away.
func TestNativeOverlayDoesNotStartTheContainerControlPlane(t *testing.T) {
	overlay := readOverlay(t)
	if !strings.Contains(overlay, `profiles: ["container-control-plane"]`) {
		t.Fatalf("%s no longer moves the control-plane service into a profile — a native bring-up would start TWO control planes against one database", nativeOverlay)
	}
	if !strings.Contains(overlay, "depends_on: !reset {}") {
		t.Fatalf("%s no longer resets the runner's depends_on — compose refuses to start a runner that waits on a service no profile enabled", nativeOverlay)
	}
}

// TestNativeOverlayReachesTheControlPlaneByTheNameOnItsCertificate is the one that would cost an
// afternoon to rediscover. The stack CA mints exactly one SAN (cmd/cli/internal/stack/certs.go) and
// the runner pins exactly one (packages/runner/session.go), so a runner in a container cannot dial a
// native control plane by any other name. The overlay must therefore keep the name and change only
// what it resolves to.
func TestNativeOverlayReachesTheControlPlaneByTheNameOnItsCertificate(t *testing.T) {
	overlay := readOverlay(t)
	certs, err := os.ReadFile("../../cmd/cli/internal/stack/config.go")
	if err != nil {
		t.Fatalf("read the stack config: %v", err)
	}
	if !strings.Contains(string(certs), `controllerDNS = "control-plane"`) {
		t.Fatal("the stack's controllerDNS constant moved — the overlay's host-gateway alias is pinned to the old name")
	}
	if !strings.Contains(overlay, `"control-plane:host-gateway"`) {
		t.Fatalf("%s no longer aliases control-plane to the host gateway — the runner would resolve the name inside the compose network and find nothing", nativeOverlay)
	}
	if !strings.Contains(overlay, `PALAI_CONTROLLER_URL: "https://control-plane:${PALAI_RUNNER_PORT}"`) {
		t.Fatalf("%s no longer dials the control plane by its certificate name; a different host in the URL fails the runner's single-SAN pin", nativeOverlay)
	}
}

// TestNativeOverlayBindsTheWorkspaceAtTheSameAbsolutePath pins the property the whole coding path
// rests on: the control plane hands the runner a path, and the runner hands it to the Docker daemon
// as a bind source, which the daemon resolves on the HOST. Source and target must be the same string
// or a run's workspace is mounted somewhere the control plane never named.
func TestNativeOverlayBindsTheWorkspaceAtTheSameAbsolutePath(t *testing.T) {
	if !strings.Contains(readOverlay(t), `"${PALAI_WORKSPACE_ROOT}:${PALAI_WORKSPACE_ROOT}"`) {
		t.Fatalf("%s no longer binds the workspace root at the identical absolute path on both sides", nativeOverlay)
	}
}
