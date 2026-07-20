package runner

import (
	"os"
	"path/filepath"
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
// an arbitrary host path. A path inside the root passes; a sibling or a traversal outside is
// rejected; an empty root or path disables the check.
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
	// No configured root, or no workspace, disables the check (pre-E09 behaviour).
	if err := workspaceUnderRoot(outside, ""); err != nil {
		t.Fatalf("empty root should disable the check: %v", err)
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
