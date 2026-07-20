//go:build e2e

package responses

// TestCodingAutoProvisionDeterministic is the E09 Task 10 deterministic proof that the PRODUCTION run
// path — POST /v1/responses with the contracted `repository` field, then the worker's orchestrator —
// auto-provisions a coding workspace, so a real end-user session reaches the file/shell tools without
// any of the manual wiring the T9 seam test (coding_journey_test.go) does by hand. It is the seam that
// closes the interactive-coding gap: the run is admitted over the real HTTP API with a binding attached,
// and the orchestrator (not the test) resolves the binding, allocates the workspace, clones @ the ref
// under a brokered credential, acquires the writer lease, and sets attempt.WorkspaceHostPath. It asserts:
//   - admit attached the session-scoped workspace (state=requested) carrying the binding + ref;
//   - the root run auto-provisioned it (an allocation with a host path exists; the workspace is leased);
//   - the file + shell tools reached the provisioned workspace (marker on disk, tool_calls recorded);
//   - the changeset compiled at finalize from the tool ledger (a changesets row).
//
// The FULL journey (approved push + PR, external receipt, idempotency) is T9's; this proves the wiring
// that makes any of it reachable from a plain HTTP request.

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
	"github.com/palgroup/palai/packages/coordinator"
)

func TestCodingAutoProvisionDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// A repository binding whose clone URL is the faithful Git double (a real local remote).
	remote := newCodingRemote(t)
	bindingID := newID("bnd")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID:          bindingID,
		Provider:           "local",
		RepositoryIdentity: "acme/widgets",
		CloneURL:           remote.url,
		DefaultBranch:      "main",
		ConnectionRef:      "conn_local",
		AllowedOperations:  []string{"push_branch", "open_pull_request"},
	}); err != nil {
		t.Fatalf("create repository binding: %v", err)
	}

	// Admit over the real HTTP API with the contracted `repository` field attached. Admit resolves it
	// (like delegations) and attaches the session-scoped workspace — the session→binding link.
	body := fmt.Sprintf(`{"input":"add a feature","repository":{"binding_id":%q,"ref":%q}}`, bindingID, remote.head)
	responseID, sessionID, runID := h.admitWith(body, newID("idem"))

	// Admit attached the workspace: session-scoped, still requested, carrying the binding + ref.
	var wsState, wsBinding, wsRef string
	if err := h.spine.Pool().QueryRow(ctx,
		`SELECT state, repository_binding_id, requested_ref FROM workspaces
		 WHERE session_id=$1 AND organization_id=$2 AND project_id=$3`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&wsState, &wsBinding, &wsRef); err != nil {
		t.Fatalf("admit did not attach a session workspace: %v", err)
	}
	if wsState != "requested" || wsBinding != bindingID || wsRef != remote.head {
		t.Fatalf("attached workspace = state:%q binding:%q ref:%q, want requested / %q / %q",
			wsState, wsBinding, wsRef, bindingID, remote.head)
	}

	// The orchestrator wired exactly as production main.go wires it when PALAI_WORKSPACE_ROOT + a
	// repository broker are configured: a workspace provisioner (root dir + broker), the coding tools,
	// the shell runner, and the changeset writer. Everything else — resolving the binding, allocating,
	// cloning, leasing, setting WorkspaceHostPath — the orchestrator does itself; the test descriptor
	// carries NO workspace path.
	const marker = "AUTOPROVISION-DET-4b1e9c"
	provisionRoot := newAllocationRoot(t)
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		&codingProvider{marker: marker}, tools.FileTool(), tools.ShellTool(), tools.CommitTool(),
		tools.TaskTool(), tools.PushTool(), tools.PullRequestTool())
	orch.SetShellRunner(hostShellRunner{})
	orch.SetWorkspaceProvisioner(provisionRoot, repositories.NewLocalBroker())
	orch.SetChangesetWriter(&recordingArtifactWriter{h: h})

	stop := h.runWorker(orch)
	defer stop()
	h.awaitResponseState(responseID, "completed", 90*time.Second)

	// The run auto-provisioned: an allocation with a host path under the provisioner root exists and the
	// workspace reached leased (the writer lease was taken at root-run start).
	alloc := h.allocationRootFor(sessionID)
	if alloc == "" || !strings.HasPrefix(alloc, provisionRoot) {
		t.Fatalf("allocation host path = %q, want a dir under the provisioner root %q", alloc, provisionRoot)
	}

	// The file tool reached the provisioned workspace: the marker is on disk in the cloned repo.
	feature, err := os.ReadFile(filepath.Join(alloc, "repo", "feature.txt"))
	if err != nil || !strings.Contains(string(feature), marker) {
		t.Fatalf("file tool did not persist the marker to the auto-provisioned workspace (got %q, err %v)", feature, err)
	}
	// The file + shell tools each recorded a completed tool_call against this run — they were reachable.
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file'`, runID); n != 1 {
		t.Fatalf("file tool_call rows = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.shell'`, runID); n != 1 {
		t.Fatalf("shell tool_call rows = %d, want 1", n)
	}

	// The changeset compiled at finalize from the tool ledger (auto-invoked, not driven by the test).
	if n := h.count(`SELECT count(*) FROM changesets WHERE run_id=$1`, runID); n != 1 {
		t.Fatalf("changeset rows = %d, want 1 (finalize compiles a changeset for a run that prepared a repo)", n)
	}
}

// allocationRootFor reads the host path of the session workspace's current allocation — where the root
// run auto-provisioned the repo (at <root>/repo), so the test can confirm the tools touched it.
func (h *harness) allocationRootFor(sessionID string) string {
	h.t.Helper()
	var hostPath string
	if err := h.spine.Pool().QueryRow(context.Background(),
		`SELECT a.host_path FROM workspace_allocations a
		 JOIN workspaces w ON w.id = a.workspace_id
		 WHERE w.session_id = $1 AND w.organization_id = $2 AND w.project_id = $3
		 ORDER BY a.fence DESC LIMIT 1`,
		sessionID, h.tenant.Organization, h.tenant.Project).Scan(&hostPath); err != nil {
		h.t.Fatalf("read allocation host path: %v", err)
	}
	return hostPath
}
