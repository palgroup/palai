package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
)

const validLeaseDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// validLeaseLimits is a well-formed bounds set so ParseLeaseOffer's limit validation passes and the
// test exercises the workspace projection, not the bounds.
func validLeaseLimits() Limits {
	return Limits{
		WallTimeMS: 60000, MaxStdoutBytes: 1 << 20, MaxStderrBytes: 1 << 16,
		MaxFrameBytes: 1 << 20, MaxMemoryBytes: 1 << 28, MaxProcessCount: 64,
	}
}

// TestParseLeaseOfferCarriesWorkspace proves the FLAG A wire: a lease.offer that carries the
// workspace allocation projects it onto the Lease, so serveLease can bind-mount it to /workspace. A
// workspace-less offer projects an empty path — the pre-E09 behaviour.
func TestParseLeaseOfferCarriesWorkspace(t *testing.T) {
	offer := contracts.RunnerMessage{
		Protocol:  RunnerProtocolV1,
		Type:      "lease.offer",
		LeaseID:   "lease_att_x",
		RunID:     "run_wsflag",
		AttemptID: "att_wsflag",
		Fence:     3,
		Data: map[string]any{
			"image_digest":        validLeaseDigest,
			"limits":              validLeaseLimits(),
			"workspace_host_path": "/srv/palai/ws/alloc-1",
			"workspace_read_only": true,
			"workspace_unsafe":    false,
		},
	}
	lease, err := ParseLeaseOffer(offer)
	if err != nil {
		t.Fatalf("ParseLeaseOffer() error = %v", err)
	}
	if lease.WorkspaceHostPath != "/srv/palai/ws/alloc-1" || !lease.WorkspaceReadOnly || lease.WorkspaceUnsafe {
		t.Fatalf("workspace fields not projected: %+v", lease)
	}

	bare := offer
	bare.Data = map[string]any{"image_digest": validLeaseDigest, "limits": validLeaseLimits()}
	leaseBare, err := ParseLeaseOffer(bare)
	if err != nil {
		t.Fatalf("ParseLeaseOffer(bare) error = %v", err)
	}
	if leaseBare.WorkspaceHostPath != "" || leaseBare.WorkspaceReadOnly || leaseBare.WorkspaceUnsafe {
		t.Fatalf("workspace-less offer projected a workspace: %+v", leaseBare)
	}
}

// TestWorkspaceUnderRoot proves carry (b): a lease's workspace path must sit under the runner's
// managed allocation root before it is bind-mounted, so a control plane cannot make the runner mount
// an arbitrary host path. A path inside the root passes; a sibling or a traversal outside is rejected;
// an empty PATH is a workspace-less lease and passes.
//
// AN EMPTY ROOT USED TO PASS HERE AND NOW REFUSES. That assertion is not deleted, it is INVERTED, and the
// inversion is kept in this test rather than only in the new one because a reader who remembers the old
// behaviour should meet the correction where they expect the old line. The reason is measured and lives in
// serve.go'"'"'s header: no shipped file ever gave a runner PALAI_WORKSPACE_ROOT, so "an empty root disables
// the check" described every deployment rather than an old one, which made the check dead code everywhere.
// TestAnUnsetAllocationRootRefusesAWorkspaceBearingLease is the full argument.
func TestWorkspaceUnderRoot(t *testing.T) {
	root := t.TempDir()
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	inside := filepath.Join(realRoot, "alloc-1")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatalf("mkdir inside: %v", err)
	}
	outside := t.TempDir() // a sibling root, deliberately not under realRoot

	if err := workspaceUnderRoot(inside, realRoot); err != nil {
		t.Fatalf("path inside the root was rejected: %v", err)
	}
	if err := workspaceUnderRoot(outside, realRoot); err == nil {
		t.Fatal("a path outside the allocation root was accepted; the runner would mount an arbitrary host path")
	}
	if err := workspaceUnderRoot(filepath.Join(realRoot, "..", "escape"), realRoot); err == nil {
		t.Fatal("a traversal above the root was accepted")
	}
	// An unset root REFUSES a lease that carries a path — see the header. A workspace-less lease still
	// passes, with or without a root: there is nothing to place, so there is nothing to refuse.
	if err := workspaceUnderRoot(outside, ""); err == nil {
		t.Fatal("an unset allocation root admitted a workspace path; the §24 boundary is off on exactly the " +
			"configuration every shipped compose file produces")
	}
	if err := workspaceUnderRoot("", realRoot); err != nil {
		t.Fatalf("empty path (workspace-less lease) should pass: %v", err)
	}
}

// TestAdmitWorkspaceMountRequiresRunnerOptInForUnsafeBind proves the §30.13 unsafe-bind trust
// boundary (§24): a control plane setting WorkspaceUnsafe does NOT by itself let the runner mount an
// arbitrary host path — the runner's own operator must also opt in. A normal allocation still goes
// through the under-root check.
func TestAdmitWorkspaceMountRequiresRunnerOptInForUnsafeBind(t *testing.T) {
	root := t.TempDir()
	inside := filepath.Join(root, "alloc-1")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := admitWorkspaceMount(Lease{WorkspaceHostPath: inside}, root, false); err != nil {
		t.Fatalf("under-root allocation rejected: %v", err)
	}
	if err := admitWorkspaceMount(Lease{WorkspaceHostPath: t.TempDir()}, root, false); err == nil {
		t.Fatal("allocation outside the runner root admitted; want rejected")
	}
	unsafe := Lease{WorkspaceHostPath: "/anywhere/on/host", WorkspaceUnsafe: true}
	if err := admitWorkspaceMount(unsafe, root, false); err == nil {
		t.Fatal("unsafe bind admitted without runner opt-in; a control plane alone escalated to an arbitrary host mount")
	}
	if err := admitWorkspaceMount(unsafe, root, true); err != nil {
		t.Fatalf("unsafe bind rejected despite runner opt-in: %v", err)
	}
}

// TestAnUnsetAllocationRootRefusesAWorkspaceBearingLease is the §24 boundary closing on the one
// configuration every deployment in this tree actually has.
//
// WHAT WAS MEASURED, 2026-08-01. `workspaceUnderRoot` returned nil for an empty root — "an empty root
// disables the check (no managed root configured)" — and NO SHIPPED FILE GIVES A RUNNER THAT ROOT:
//
//	runner `environment:` block carries PALAI_WORKSPACE_ROOT?
//	  deploy/compose/compose.yaml               NO
//	  deploy/compose/production.yml             NO
//	  deploy/compose/native-control-plane.yml   NO   <- binds the PATH as a volume, sets no variable
//	  deploy/airgap/airgap.yml                  NO
//	docker inspect <live runner> | grep -c PALAI_WORKSPACE_ROOT   -> 0
//
// So the branch that "disables the check" was not a compatibility path for old deployments. It was the
// path EVERY deployment took, which made admitWorkspaceMount's non-unsafe arm dead code and the comment
// above it — "so a control plane cannot make the runner mount an arbitrary host path such as /etc" —
// false everywhere.
//
// IT WAS NOT EXPLOITABLE AND THAT IS THE POINT. Workspaces are off on compose (`GET /v1/capabilities`
// reports `workspaces = unavailable`, because the CONTROL PLANE's root is unset too), so no lease carries
// a WorkspaceHostPath at all. The hole ARMS ITSELF when an operator turns the feature on: main.go:677
// starts provisioning when the control plane's PALAI_WORKSPACE_ROOT is set, and nothing requires the
// runner's. One variable name, two planes, two meanings — set the one that makes the feature work and the
// one that guards it stays silent.
//
// SO AN EMPTY ROOT NOW REFUSES A LEASE THAT CARRIES A PATH. A runner that cannot tell whether a path is
// inside its managed root has no basis on which to mount it, and the fail-open direction hands that
// decision to the control plane — which is the trust boundary §24 draws. A workspace-LESS lease is
// untouched: it has nothing to check and it is what every non-coding run sends.
func TestAnUnsetAllocationRootRefusesAWorkspaceBearingLease(t *testing.T) {
	somewhere := t.TempDir()

	err := workspaceUnderRoot(somewhere, "")
	if err == nil {
		t.Fatal("a lease carrying a workspace path was admitted by a runner with NO managed allocation root. " +
			"The under-root check is the §24 boundary that stops a control plane naming an arbitrary host path, " +
			"and an unset PALAI_WORKSPACE_ROOT on the runner switches it off — which is the configuration every " +
			"shipped compose file produces")
	}
	// THE MESSAGE HAS TO NAME THE VARIABLE. "unexpected nil" or "invalid root" sends an operator to read Go
	// source; the whole reason this deployment surface exists is that the answer was only in `docker inspect`.
	if !strings.Contains(err.Error(), "PALAI_WORKSPACE_ROOT") {
		t.Errorf("the refusal does not name the variable that would fix it: %v", err)
	}

	// A WORKSPACE-LESS LEASE IS UNTOUCHED. Every non-coding run sends one, so refusing here would take the
	// product down rather than close a boundary.
	if err := workspaceUnderRoot("", ""); err != nil {
		t.Errorf("a lease with no workspace was refused by a runner with no root: %v", err)
	}

	// AND THE SAME THROUGH THE ADMISSION FUNCTION, which is what serve.go actually calls — a check that is
	// right in a helper and unreachable from the caller is this tree's most-found defect.
	if err := admitWorkspaceMount(Lease{WorkspaceHostPath: somewhere}, "", false); err == nil {
		t.Fatal("admitWorkspaceMount admitted a workspace-bearing lease with no allocation root")
	}
	// The unsafe arm is unchanged and still needs the runner's own opt-in: it is a DIFFERENT decision, made
	// explicitly by the operator, and it does not become weaker because the ordinary path got stricter.
	unsafe := Lease{WorkspaceHostPath: somewhere, WorkspaceUnsafe: true}
	if err := admitWorkspaceMount(unsafe, "", false); err == nil {
		t.Error("an unsafe bind was admitted with no runner opt-in")
	}
	if err := admitWorkspaceMount(unsafe, "", true); err != nil {
		t.Errorf("an opted-in unsafe bind was refused because the root is empty; the unsafe arm never consulted "+
			"the root and must not start now: %v", err)
	}
}
