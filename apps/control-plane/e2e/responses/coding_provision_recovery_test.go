//go:build e2e

package responses

// E09 Task 10 review blockers 1 + 2: provisioning must not leak a writer lease when a state transition
// fails after the physical lease is acquired (blocker 1 — no TTL, so a leaked lease bricks the session
// forever), and a workspace left mid-provision by a failed clone must recover on retry rather than stay
// stuck in `preparing` (blocker 2). Both are reproduced by putting the session workspace in an
// inconsistent state a real crash would leave, then driving a run through the production provisioning.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	statemachines "github.com/palgroup/palai/packages/state-machines"

	"github.com/palgroup/palai/storage"
)

// codingProvisionOrch builds an orchestrator wired exactly as production does for a coding stack: the
// workspace provisioner (root dir + local broker), the coding tools, a host shell runner, and a
// recording changeset writer.
func codingProvisionOrch(h *harness, marker, provisionRoot string) *execution.Orchestrator {
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		&codingProvider{marker: marker}, tools.FileTool(), tools.ShellTool(), tools.CommitTool(),
		tools.TaskTool(), tools.PushTool(), tools.PullRequestTool())
	orch.SetShellRunner(hostShellRunner{})
	orch.SetWorkspaceProvisioner(provisionRoot, repositories.NewLocalBroker())
	orch.SetChangesetWriter(&recordingArtifactWriter{h: h})
	return orch
}

// workspaceIDFor reads the session's attached workspace id.
func (h *harness) workspaceIDFor(sessionID string) string {
	h.t.Helper()
	var id string
	if err := h.spine.Pool().QueryRow(storage.WithSystemScope(context.Background()),
		`SELECT id FROM workspaces WHERE session_id=$1 AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&id); err != nil {
		h.t.Fatalf("read workspace id: %v", err)
	}
	return id
}

// TestCodingProvisionNoLeaseLeakOnStuckState (blocker 1): a workspace left `leased` with NO active
// physical lease — the inconsistency a crash between ReleaseWriterLease and AdvanceWorkspace(Release)
// leaves — must NOT leak the lease the retry acquires. The retry's AdvanceWorkspace(Lease) is invalid
// from `leased`; the fix releases the just-acquired lease on that failure (or tolerates it and keeps the
// valid lease) so the run still completes and leaves exactly zero active leases.
func TestCodingProvisionNoLeaseLeakOnStuckState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	const marker = "PROVISION-LEASE-DET-3c9a"

	remote := newCodingRemote(t)
	bindingID := newID("bnd")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "acme/widgets",
		CloneURL: remote.url, DefaultBranch: "main", ConnectionRef: "conn_local",
	}); err != nil {
		t.Fatalf("create repository binding: %v", err)
	}

	provisionRoot := newAllocationRoot(t)

	// First response: a clean run that provisions + clones the workspace, leaving it `ready` with a valid
	// allocation. codingProvider only needs the file/shell/commit tools reachable.
	body := fmt.Sprintf(`{"input":"edit","repository":{"binding_id":%q,"ref":%q}}`, bindingID, remote.head)
	resp1, sessionID, _ := h.admitWith(body, newID("idem"))
	stop := h.runWorker(codingProvisionOrch(h, marker, provisionRoot))
	h.awaitResponseState(resp1, "completed", 90*time.Second)
	stop()

	// Simulate the crash-between inconsistency: flip the workspace to `leased` while NO physical lease is
	// active (the first run released its physical lease on completion). The state projection now lies.
	wsID := h.workspaceIDFor(sessionID)
	if err := h.spine.AdvanceWorkspace(ctx, h.tenant, wsID, statemachines.WorkspaceCmdLease); err != nil {
		t.Fatalf("force leased state: %v", err)
	}

	// Second response (chained into the same session): the retry sees `leased`, reuses the allocation, and
	// acquires a lease. Without the fix, its AdvanceWorkspace(Lease) fails and the lease LEAKS.
	resp2, _, run2 := h.admitWith(fmt.Sprintf(`{"input":"again","session_id":%q}`, sessionID), newID("idem"))
	stop2 := h.runWorker(codingProvisionOrch(h, marker, provisionRoot))
	defer stop2()
	h.awaitResponseState(resp2, "completed", 90*time.Second)

	// No leaked lease: after the run completes, the workspace has zero active writer leases.
	if n := h.count(
		`SELECT count(*) FROM workspace_leases l JOIN workspaces w ON w.id=l.workspace_id
		 WHERE w.session_id=$1 AND l.state='active'`, sessionID); n != 0 {
		t.Fatalf("active writer leases after completion = %d, want 0 (a failed transition leaked the lease)", n)
	}
	if st := h.runState(run2); st != "completed" {
		t.Fatalf("retry run state = %q, want completed (the stuck-leased workspace bricked it)", st)
	}
}

// TestCodingProvisionRecoversFromStuckPreparing (blocker 2): a workspace left in `preparing` by a failed
// clone (an allocation exists but has no usable repo) must recover on the next run — re-provision fresh —
// rather than reuse the partial allocation and error on Head() forever.
func TestCodingProvisionRecoversFromStuckPreparing(t *testing.T) {
	h := newHarness(t)
	ctx := storage.WithTenant(context.Background(), h.tenant.Organization, h.tenant.Project)
	const marker = "PROVISION-RECOVER-DET-8d21"

	remote := newCodingRemote(t)
	bindingID := newID("bnd")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "acme/widgets",
		CloneURL: remote.url, DefaultBranch: "main", ConnectionRef: "conn_local",
	}); err != nil {
		t.Fatalf("create repository binding: %v", err)
	}

	body := fmt.Sprintf(`{"input":"edit","repository":{"binding_id":%q,"ref":%q}}`, bindingID, remote.head)
	resp, sessionID, runID := h.admitWith(body, newID("idem"))

	// Simulate a crashed mid-provision: drive the workspace to `preparing` and mint an allocation whose
	// repo dir is EMPTY (the clone never finished). The old code's reuse path would Head() this and error.
	wsID := h.workspaceIDFor(sessionID)
	if err := h.spine.AdvanceWorkspace(ctx, h.tenant, wsID, statemachines.WorkspaceCmdProvision); err != nil {
		t.Fatalf("advance provisioning: %v", err)
	}
	if err := h.spine.AdvanceWorkspace(ctx, h.tenant, wsID, statemachines.WorkspaceCmdPrepare); err != nil {
		t.Fatalf("advance preparing: %v", err)
	}
	partial := filepath.Join(newAllocationRoot(t), "partial")
	if err := os.MkdirAll(filepath.Join(partial, "repo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := h.spine.AllocateWorkspace(ctx, "alloc_"+newID("partial"), wsID, partial); err != nil {
		t.Fatalf("mint partial allocation: %v", err)
	}

	provisionRoot := newAllocationRoot(t)
	stop := h.runWorker(codingProvisionOrch(h, marker, provisionRoot))
	defer stop()
	h.awaitResponseState(resp, "completed", 90*time.Second)

	if st := h.runState(runID); st != "completed" {
		t.Fatalf("run state = %q, want completed (the stuck-preparing workspace never recovered)", st)
	}
	// A fresh allocation was minted under the provisioner root and cloned into.
	alloc := h.allocationRootFor(sessionID)
	if !strings.HasPrefix(alloc, provisionRoot) {
		t.Fatalf("current allocation = %q, want a fresh dir under the provisioner root %q (did not re-provision)", alloc, provisionRoot)
	}
	if _, err := os.Stat(filepath.Join(alloc, "repo", "feature.txt")); err != nil {
		t.Fatalf("recovered workspace missing the edited file: %v", err)
	}
}
