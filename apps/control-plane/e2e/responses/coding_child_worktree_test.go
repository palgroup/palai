//go:build e2e

package responses

// TestCodingChildIsolatedWorktreeDeterministic proves the E09 Task 10 close of the child_dispatch.go:47
// ponytail: a delegated child that asks for an ISOLATED workspace edits a copy-on-write git worktree off
// the parent's auto-provisioned checkout — on its OWN branch, WITHOUT taking a second writer lease. The
// parent's own checkout is never touched by the child; the child's edits reach it only through an
// explicit merge (not exercised here — REP-011 is proven by the repositories worktree unit tests). The
// load-bearing assertions are: the child's file landed in the CHILD worktree, not the parent repo, and
// the workspace has exactly ONE active writer lease (the root run's) — the child took none.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

func TestCodingChildIsolatedWorktreeDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	remote := newCodingRemote(t)
	bindingID := newID("bnd")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "acme/widgets",
		CloneURL: remote.url, DefaultBranch: "main",
	}); err != nil {
		t.Fatalf("create repository binding: %v", err)
	}

	const childMarker = "CHILD-WORKTREE-DET-7a2f"
	body := fmt.Sprintf(`{"input":"delegate a coder","repository":{"binding_id":%q,"ref":%q}}`, bindingID, remote.head)
	responseID, sessionID, parentRunID := h.admitWith(body, newID("idem"))

	provisionRoot := newAllocationRoot(t)
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		codingChildProvider{childModel: "fake-child", childMarker: childMarker}, tools.FileTool())
	orch.SetWorkspaceProvisioner(provisionRoot, repositories.NewLocalBroker())

	stop := h.runWorker(orch)
	defer stop()
	h.awaitResponseState(responseID, "completed", 90*time.Second)

	childRun, _ := h.childRunOf(parentRunID)
	if state := h.runState(childRun); state != "completed" {
		t.Fatalf("child run state = %q, want completed", state)
	}

	// The child edited its OWN worktree (parent-allocation/children/<child>/repo), not the parent repo.
	parentAlloc := h.allocationRootFor(sessionID)
	childFile := filepath.Join(parentAlloc, "children", childRun, "repo", "child.txt")
	got, err := os.ReadFile(childFile)
	if err != nil || !strings.Contains(string(got), childMarker) {
		t.Fatalf("child worktree file = %q (err %v), want the marker in the child's own worktree", got, err)
	}
	if _, err := os.Stat(filepath.Join(parentAlloc, "repo", "child.txt")); !os.IsNotExist(err) {
		t.Fatalf("child.txt leaked into the PARENT repo (err %v) — the isolated worktree did not isolate", err)
	}

	// The child took NO writer lease — ever. The workspace has exactly ONE lease across its whole life
	// (the root run's, released at completion); the child branched the parent's checkout copy-on-write
	// without a second single-writer slot.
	if n := h.count(
		`SELECT count(*) FROM workspace_leases l JOIN workspaces w ON w.id=l.workspace_id
		 WHERE w.session_id=$1`, sessionID); n != 1 {
		t.Fatalf("total writer leases = %d, want exactly 1 (only the root run's; the child takes none)", n)
	}
	if n := h.count(`SELECT count(*) FROM workspace_leases WHERE run_id=$1`, childRun); n != 0 {
		t.Fatalf("child took %d writer leases, want 0 (an isolated worktree needs no lease)", n)
	}
	// The one lease is the root run's.
	if n := h.count(`SELECT count(*) FROM workspace_leases WHERE run_id=$1`, parentRunID); n != 1 {
		t.Fatalf("root run writer leases = %d, want 1", n)
	}
}

// codingChildProvider drives a parent that delegates an ISOLATED coder child and a child that writes a
// file into its own worktree before finishing. The parent finishes once the child's marker answer folds
// back. It is the child-workspace mirror of agentDelegatingProvider (which delegates a workspace-less
// researcher).
type codingChildProvider struct{ childModel, childMarker string }

func (p codingChildProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: req.Model,
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	if req.Model == p.childModel {
		// The child: write a file into its isolated worktree, then finish with the marker.
		toolResults := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				toolResults++
			}
		}
		if toolResults == 0 {
			res.ProviderRequestID = "prov_child_file"
			res.ToolCalls = []modelbroker.ToolCall{{
				ID: "call_child_file", Name: "palai.workspace.file",
				Arguments: fmt.Sprintf(`{"op":"write","path":"repo/child.txt","content":%q}`, p.childMarker+"\n"),
			}}
			res.FinishReason = "tool_calls"
			return res, nil
		}
		res.ProviderRequestID = "prov_child_final"
		res.Output = p.childMarker
		res.FinishReason = "stop"
		return res, nil
	}
	// The parent: finish once the child's answer folded in.
	for _, m := range req.Messages {
		if strings.Contains(m.Content, p.childMarker) {
			res.ProviderRequestID = "prov_parent_final"
			res.Output = "parent folded the child result"
			res.FinishReason = "stop"
			return res, nil
		}
	}
	// The parent's first step delegates an ISOLATED coder child (model-driven).
	res.ProviderRequestID = "prov_parent_delegate"
	res.ToolCalls = []modelbroker.ToolCall{{
		ID: "call_agent", Name: "agent",
		Arguments: fmt.Sprintf(`{"role":"coder","objective":"edit","model":%q,"workspace_mode":"isolated","required":true}`, p.childModel),
	}}
	res.FinishReason = "tool_calls"
	return res, nil
}
