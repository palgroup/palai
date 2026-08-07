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

// TestNativeOverlayDoesNotStartTheContainerRunner is the same shape as the control-plane profile above
// and it arrived for a sharper reason (A.3 T4): a run's shell command executes on the machine holding
// the attempt's lease, so a runner in Docker is a Linux box with no Xcode — which is the entire point of
// this posture. `palai up --native` runs the runner as a process on this machine instead.
//
// A PROFILE RATHER THAN A REMOVAL, again: the definition stays for `--profile container-runner`, the A/B
// an operator wants when the native one misbehaves. What must never happen is both at once — two
// machines in one pool means a command lands on whichever the gateway hands out, and "where did this
// run" stops having an answer.
func TestNativeOverlayDoesNotStartTheContainerRunner(t *testing.T) {
	overlay := readOverlay(t)
	if !strings.Contains(overlay, `profiles: ["container-runner"]`) {
		t.Fatalf("%s no longer moves the runner service into a profile — a native bring-up would put TWO machines "+
			"in one pool and a shell command would land on whichever the gateway handed out", nativeOverlay)
	}
	// The bring-up's own list is the other direction of the same fact: this walk finds what the file
	// says, and that list finds what the command does.
	up, err := os.ReadFile("../../cmd/cli/internal/stack/native.go")
	if err != nil {
		t.Fatalf("read the native bring-up: %v", err)
	}
	if !strings.Contains(string(up), `func nativeComposeServices() []string { return []string{"postgres", "object-store"} }`) {
		t.Fatal("the native bring-up's compose service list is not the one this guard knows: re-read it and check " +
			"the runner is still absent from what `palai up --native` starts")
	}
}

// TestNativeOverlayReachesTheControlPlaneByTheNameOnItsCertificate is the one that would cost an
// afternoon to rediscover. The trust root mints exactly one SAN (packages/localca) and the runner pins
// exactly one (packages/runner/session.go), so a runner in a container cannot dial a native control
// plane by any other name. The overlay must therefore keep the name and change only what it resolves to.
//
// IT READ cmd/cli/internal/stack UNTIL 2026-08-07, and the move is the point rather than a detail: the
// certificate carrying this name is now minted by the CONTROL PLANE at boot, so a deployment that never
// installs the CLI still serves it. This guard follows the minter, because the minter is what decides
// which name the runner will be asked to trust.
func TestNativeOverlayReachesTheControlPlaneByTheNameOnItsCertificate(t *testing.T) {
	overlay := readOverlay(t)
	certs, err := os.ReadFile("../../packages/localca/localca.go")
	if err != nil {
		t.Fatalf("read the trust-root minter: %v", err)
	}
	if !strings.Contains(string(certs), `ControllerDNS = "control-plane"`) {
		t.Fatal("localca.ControllerDNS moved — the overlay's host-gateway alias is pinned to the old name")
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

// TestTheNativeOverlayGivesTheRunnerTheRootItBindsIntoIt is the fourth property, and it was missing for
// as long as the overlay has existed.
//
// THE BIND AND THE VARIABLE DO DIFFERENT JOBS. The volume above makes the allocation root REACHABLE
// inside the runner container. PALAI_WORKSPACE_ROOT in the runner's own environment is what makes the
// runner CHECK it: packages/runner's workspaceUnderRoot refuses to bind-mount a leased path that does not
// sit under this root — the §24 boundary against a control plane naming an arbitrary host path such as
// /etc.
//
// Until this guard existed the overlay shipped the mount and not the variable, so the runner's root was
// EMPTY. Measured 2026-08-01, across every shipped file:
//
//	runner `environment:` block carries PALAI_WORKSPACE_ROOT?   compose.yaml NO, production.yml NO,
//	native-control-plane.yml NO, airgap.yml NO;  docker inspect <live runner> -> 0
//
// and an empty root used to DISABLE the check. So the boundary was off on the one shipped posture where
// workspaces work at all. serve.go now refuses instead of admitting, which turns that silence into a
// failed run — the safe direction, and still a broken deployment unless this line is here.
//
// IT ASSERTS THE VALUES ARE THE SAME EXPRESSION, not merely that both exist. A runner checking against a
// different root than the control plane allocates under refuses every coding run, which is a stack that
// looks configured and works for nothing.
func TestTheNativeOverlayGivesTheRunnerTheRootItBindsIntoIt(t *testing.T) {
	overlay := readOverlay(t)
	const envLine = `PALAI_WORKSPACE_ROOT: "${PALAI_WORKSPACE_ROOT}"`
	if !strings.Contains(overlay, envLine) {
		t.Fatalf("%s binds ${PALAI_WORKSPACE_ROOT} into the runner and does not set it in the runner's "+
			"environment. The bind makes the path reachable; the VARIABLE is what makes the runner refuse a "+
			"leased path outside it (packages/runner workspaceUnderRoot, §24). Without it the runner's root is "+
			"empty, and this is the only shipped posture where workspaces run", nativeOverlay)
	}
	// The env line must sit in the RUNNER's block, not the control-plane's — the control plane is not a
	// compose service in this overlay at all, so a stray copy there would configure nothing while reading
	// as if it did.
	runner := overlay[strings.Index(overlay, "  runner:"):]
	if !strings.Contains(runner, envLine) {
		t.Fatalf("%s sets PALAI_WORKSPACE_ROOT outside the runner service block", nativeOverlay)
	}
	if !strings.Contains(runner, `"${PALAI_WORKSPACE_ROOT}:${PALAI_WORKSPACE_ROOT}"`) {
		t.Fatalf("%s no longer binds the root into the same runner it configures", nativeOverlay)
	}
}
