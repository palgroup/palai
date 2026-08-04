//go:build component

package execution

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/storage"
)

// THE TERMINAL IS A BOUNDARY, and until this file nothing proved it had to be.
//
// MEASURED END TO END on the live native stack, 2026-08-02, against a real local git remote. An operator
// approved a push through POST /v1/approvals/{id}/approve. The session journal, in order:
//
//	command.accepted.v1 (approve) -> approval.approved.v1 -> command.applied.v1
//	-> run.running.v1  (the parked run WOKE)
//	-> attempt.recovering.v1 -> model_step.created.v1 ... model_step.completed.v1
//	-> run.completed.v1
//
// The publication row stayed `approved`, carried no receipt and NO WARNING, and `git ls-remote` on the
// remote was byte-identical before and after. Every layer behaved as designed.
//
// WHY: pumpApprovedPublications lives inside pumpCommands, and the run loop only reaches pumpCommands
// `if continues` — the INPUT boundary before the NEXT model request. A woken attempt whose next step is a
// FINAL answer has no next request, therefore no boundary, therefore no pump. The run then ends, and
// nothing will ever run it again: the approval is spent and the write never happens.
//
// This is the SECOND gate between a human's Approve and a branch on a remote. The first was the publisher
// being constructed inside the GitHub App gate. Fixing only the first leaves the operator exactly where
// they started on the path they actually walk, which is why this test drives the WHOLE attempt through
// ExecuteAttempt rather than calling the pump — a test that called publishApproved would have passed
// before the fix and after it, and measured nothing about reachability.

// terminalPublishFixture is one tenant, one run with a repository binding that carries its OWN
// credential, a real workspace with a commit, and a real bare remote to push to.
type terminalPublishFixture struct {
	spine                        *coordinator.Store
	repo                         *store.Store
	orch                         *Orchestrator
	tenant                       coordinator.Tenant
	sessionID, responseID, runID string
	bindingID, root, head, bare  string
	branch                       string
}

func newTerminalPublishFixture(t *testing.T) *terminalPublishFixture {
	t.Helper()
	url := os.Getenv("PALAI_COMPONENT_POSTGRES_URL")
	if url == "" {
		t.Skip("PALAI_COMPONENT_POSTGRES_URL is required; run TEST=postgres scripts/test/component")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	cs, err := coordinator.Open(ctx, url)
	if err != nil {
		t.Fatalf("coordinator.Open: %v", err)
	}
	t.Cleanup(cs.Close)
	if err := cs.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	repo, err := store.Open(ctx, url)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(repo.Close)

	f := &terminalPublishFixture{
		spine: cs, repo: repo,
		tenant:     coordinator.Tenant{Project: redeliveryID("prj")},
		sessionID:  redeliveryID("ses"),
		responseID: redeliveryID("resp"),
		runID:      redeliveryID("run"),
		bindingID:  redeliveryID("bnd"),
	}
	f.branch = "agent/" + f.sessionID + "/" + f.runID
	f.root, f.head = terminalWorkspaceWithACommit(t)
	f.bare = terminalBareRemote(t)

	sys := storage.WithSystemScope(ctx)
	for _, stmt := range [][]any{
		{`INSERT INTO organizations (id) VALUES ($1)`, f.tenant.Organization},
		{`INSERT INTO projects (id, organization_id) VALUES ($1, $2)`, f.tenant.Project, f.tenant.Organization},
		{`INSERT INTO runner_pools (id, organization_id, project_id, name, posture)
		  VALUES ($1,$2,$3,'default','unsandboxed-host')`, redeliveryID("pool"), f.tenant.Project},
		{`INSERT INTO sessions (id, organization_id, project_id) VALUES ($1,$2,$3)`, f.sessionID, f.tenant.Project},
		{`INSERT INTO responses (id, organization_id, project_id, session_id, state, input)
		  VALUES ($1,$2,$3,$4,'queued','"ship it"'::jsonb)`, f.responseID, f.tenant.Project, f.sessionID},
		{`INSERT INTO runs (id, organization_id, project_id, session_id, response_id, state)
		  VALUES ($1,$2,$3,$4,$5,'queued')`, f.runID, f.tenant.Project, f.sessionID, f.responseID},
	} {
		if _, err := cs.Pool().Exec(sys, stmt[0].(string), stmt[1:]...); err != nil {
			t.Fatalf("seed %v: %v", stmt[0], err)
		}
	}

	// THE BINDING CARRIES ITS OWN CREDENTIAL, which is the whole configuration under test: an App-less
	// deployment publishing under a tenant's panel-provisioned secret.
	if err := cs.CreateRepositoryBinding(ctx, f.tenant, coordinator.RepositoryBindingInput{
		BindingID: f.bindingID, Provider: "github", RepositoryIdentity: "local/demo-target",
		CloneURL: f.bare, DefaultBranch: "main", ConnectionRef: "demo-local-token",
	}); err != nil {
		t.Fatalf("create the repository binding: %v", err)
	}
	if err := cs.RecordPreparationReceipt(ctx, f.tenant, coordinator.PreparationReceiptInput{
		ReceiptID: redeliveryID("prep"), BindingID: f.bindingID, RunID: f.runID,
		RequestedRef: "main", BaseCommit: "0000000", TreeHash: "0000000", Branch: f.branch,
	}); err != nil {
		t.Fatalf("record the preparation receipt: %v", err)
	}

	// The orchestrator production builds, with the publisher production builds on a deployment with NO
	// GitHub App: no deployment-global broker, the connection resolver wired.
	f.orch = NewOrchestrator(repo, nil, modelbroker.New(modelbroker.Config{}), toolbroker.New())
	f.orch.SetPublisher(&RepositoryPublisher{
		ConnectionSecrets: func(ref string) ([]byte, error) {
			if org != f.tenant.Organization || ref != "demo-local-token" {
				t.Errorf("resolver asked for (org=%q ref=%q), want the binding's own", org, ref)
			}
			return []byte("tenant-provisioned-token"), nil
		},
	})
	return f
}

// approvedPush records a pending publication through the SHIPPED registry and drives it to `approved`
// through the SHIPPED decision path — AcceptCommand then ApplyApprovalDecision, which is exactly what
// store/approvals.go does for an HTTP click. A test that UPDATEd the row to 'approved' would be asserting
// against a state production never produces.
func (f *terminalPublishFixture) approvedPush(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	reg := newPublicationRegistry(f.spine, f.orch.canPublish)
	out, err := reg.RequestPublication(ctx, toolbroker.TaskScope{
		Org: f.tenant.Organization, Project: f.tenant.Project,
		SessionID: f.sessionID, RunID: f.runID, ResponseID: f.responseID,
	}, map[string]any{"operation": "push_branch", "head_sha": f.head})
	if err != nil {
		t.Fatalf("request the publication: %v", err)
	}
	pubID, _ := out["publication_id"].(string)
	hash, _ := out["request_hash"].(string)
	if pubID == "" || hash == "" {
		t.Fatalf("the publication tool answered %v", out)
	}

	commandID := redeliveryID("cmd")
	payload, _ := json.Marshal(map[string]string{"request_hash": hash, "approver": "key:operator"})
	acc, err := f.spine.AcceptCommand(ctx, f.tenant, f.sessionID, coordinator.CommandInput{
		CommandID: commandID, Kind: "approve", Payload: payload,
	})
	if err != nil || acc.State != "queued" {
		t.Fatalf("accept the approve command: (%+v, %v)", acc, err)
	}
	if _, err := f.spine.ApplyApprovalDecision(ctx, f.tenant, f.sessionID, f.responseID, f.runID,
		commandID, "approve", hash, "key:operator"); err != nil {
		t.Fatalf("apply the approval decision: %v", err)
	}
	if got := f.publicationState(t, pubID); got != "approved" {
		t.Fatalf("the publication is %q after an approve, want approved", got)
	}
	return pubID
}

func (f *terminalPublishFixture) publicationState(t *testing.T, pubID string) string {
	t.Helper()
	state, ok, err := f.spine.PublicationState(context.Background(), f.tenant, pubID)
	if err != nil || !ok {
		t.Fatalf("read the publication state: (%q,%v,%v)", state, ok, err)
	}
	return state
}

// TestAnApprovedPushPublishesOnARunWhoseNextStepIsItsLast drives a WHOLE attempt whose engine reports a
// terminal and nothing else — the shape a woken run takes when the model's next step is a final answer,
// and the shape that was measured leaving an approved push unpublished forever.
func TestAnApprovedPushPublishesOnARunWhoseNextStepIsItsLast(t *testing.T) {
	f := newTerminalPublishFixture(t)
	pubID := f.approvedPush(t)

	// The engine says nothing but "done". There is no model.request, so there is no input boundary, so
	// the only boundary this attempt ever reaches is its terminal.
	f.orch.dialer = &scriptedDialer{ch: &scriptedChannel{frames: []contracts.EngineFrame{
		engineFrame(1, "engine.ready", map[string]any{
			"selected_protocol": engineProtocol, "engine": map[string]any{"version": "test"},
		}),
		engineFrame(2, "run.terminal", map[string]any{"outcome": "completed"}),
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := f.orch.ExecuteAttempt(ctx, AttemptDescriptor{
		RunID: contracts.RunID(f.runID), AttemptID: contracts.AttemptID(redeliveryID("att")),
		Fence: 1, WorkspaceHostPath: f.root,
	}); err != nil {
		t.Fatalf("ExecuteAttempt: %v", err)
	}

	if got := f.publicationState(t, pubID); got != "published" {
		t.Fatalf("the publication is %q after the run ended, want published. The operator pressed Approve, "+
			"the run woke, the run finished — and the write never happened, with no warning on any surface", got)
	}
	// THE REMOTE'S REF, not the state column. A state that says published while the remote never moved is
	// the exact reading this whole change exists to make impossible.
	if got := terminalRemoteRef(t, f.bare, f.branch); got != f.head {
		t.Fatalf("the remote's ref is %q, want the approved head %q", got, f.head)
	}
}

// --- fixtures ----------------------------------------------------------------------------------------

func terminalWorkspaceWithACommit(t *testing.T) (root, head string) {
	t.Helper()
	root = t.TempDir()
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	if err := workspace.Prepare(root); err != nil {
		t.Fatalf("prepare the workspace allocation: %v", err)
	}
	repoDir := filepath.Join(root, workspace.RepoDir)
	run := terminalGitAt(t, repoDir)
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "PUBLISHER_GATE.md"), []byte("published under the binding's own credential\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "PUBLISHER_GATE.md")
	run("commit", "-q", "-m", "prove the App-less publish path")
	return root, run("rev-parse", "HEAD")
}

func terminalBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	terminalGitAt(t, dir)("init", "-q", "--bare", "-b", "main")
	return dir
}

func terminalRemoteRef(t *testing.T, remote, branch string) string {
	t.Helper()
	return terminalGitAt(t, remote)("rev-parse", "refs/heads/"+branch)
}

func terminalGitAt(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, strings.TrimSpace(string(out)))
		}
		return strings.TrimSpace(string(out))
	}
}
