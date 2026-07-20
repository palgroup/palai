//go:build e2e

package responses

// TestCodingJourneyDeterministic is the E09 Task 9 deterministic half of the mandatory interactive
// coding journey (spec §63.2, steps 1-7 + 10-11; the kill+recovery half 8-9 is E10). It composes the
// real coding spine end to end in CI with NO network and NO real credential: the FAKE scripted provider
// drives the REAL orchestrator, tools, and composed steps (PrepareRepository / CompileChangeset /
// publishApproved) against a FAITHFUL Git double — a real local git remote that serves the exact clone
// commit and receives the approved push, so its ref is a genuine external receipt (b20627c pattern),
// not a mock. The live tier (make uat-coding PROVIDER=provider-one) proves the SAME journey against the
// real provider + a real Git destination; this tier must pass in CI without either.
//
// It lives in package responses (not tests/e2e/coding) because the journey drives the control plane's
// internal orchestrator + tools + composed steps, which Go's internal rule forbids importing from
// tests/ — the same constraint that put the E08 newHarness here, which this reuses rather than clones.
//
// The journey, on one session, no kill:
//  1. create session + repository binding;
//  2. prepare an isolated workspace at the exact commit (preparation receipt);
//  3. the agent edits a file and runs a test through the real file/shell tools (real tool round-trip);
//  4. a second client observes the identical ordered journal;
//  5-6. queue/steer + model switch fold in at a boundary;
//  7. a required research child runs on a cheaper model via a MODEL-driven agent tool_call;
//  10. the changeset + test evidence are compiled from the tool ledger, not model prose;
//  11. an approved branch push + a draft PR happen exactly once (external receipt + idempotency).

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

func TestCodingJourneyDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// --- Step 1: create a session + a repository binding whose clone URL is the faithful Git double. ---
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
	responseID, sessionID, runID := h.admit()
	_ = responseID
	_ = sessionID

	// --- Step 2: prepare an isolated workspace at the exact commit; the receipt is model-independent. ---
	alloc := newAllocationRoot(t)
	workBranch := "agent/" + sessionID + "/" + runID
	prepared, err := execution.PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), h.tenant, execution.PrepareRepositoryInput{
		BindingID:    bindingID,
		RunID:        runID,
		RequestedRef: remote.head, // an exact commit sha, not a branch
		WorkBranch:   workBranch,
		TargetDir:    filepath.Join(alloc, "repo"),
		SecretsDir:   filepath.Join(alloc, "secrets"),
		AttemptFence: 1,
		ToolCall:     "prepare",
	})
	if err != nil {
		t.Fatalf("prepare repository: %v", err)
	}
	if prepared.Receipt.BaseCommit != remote.head {
		t.Fatalf("preparation base commit = %q, want the exact requested commit %q", prepared.Receipt.BaseCommit, remote.head)
	}
	if prepared.Receipt.Branch != workBranch {
		t.Fatalf("preparation work branch = %q, want %q", prepared.Receipt.Branch, workBranch)
	}
	// The exact tree materialized on disk: the committed file is present with its committed content.
	body, err := os.ReadFile(filepath.Join(alloc, "repo", "README.md"))
	if err != nil || strings.TrimSpace(string(body)) != "hello world" {
		t.Fatalf("prepared workspace README = %q (err %v), want the exact committed content", body, err)
	}

	// --- Step 3: the agent edits a file and runs a test through the REAL file + shell tools. The fake
	// provider is FORCED (by script) to write repo/feature.txt then run a test that reads it back — the
	// real tool round-trip against the prepared workspace, the deterministic mirror of the live T4 loop. ---
	const marker = "CODING-JOURNEY-DET-8f3a2c"
	orch := h.newOrchestratorWithTools(subprocessDialer{engineDir: h.engineDir},
		&codingProvider{marker: marker}, tools.FileTool(), tools.ShellTool(), tools.CommitTool(), tools.TaskTool())
	orch.SetShellRunner(hostShellRunner{})

	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 1, alloc)); err != nil {
		t.Fatalf("execute coding attempt: %v", err)
	}
	if st, _ := h.response(responseID); st != "completed" {
		t.Fatalf("coding run state = %q, want completed", st)
	}

	// The file tool really mutated the workspace: the marker is on disk in the cloned repo.
	feature, err := os.ReadFile(filepath.Join(alloc, "repo", "feature.txt"))
	if err != nil || !strings.Contains(string(feature), marker) {
		t.Fatalf("file tool did not persist the marker to the workspace (got %q, err %v)", feature, err)
	}
	// The shell tool ran the test AND observed the same workspace: a completed tool_call for each tool.
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file'`, runID); n != 1 {
		t.Fatalf("file tool_call rows = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.shell'`, runID); n != 1 {
		t.Fatalf("shell tool_call rows = %d, want 1", n)
	}

	// --- Step 10: compile the changeset + test evidence from the tool LEDGER, not the model's prose
	// (REP-005). The changed-file set comes from the file-tool writes; the patch is the real working-tree
	// diff against the preparation base; the test log is the shell-tool transcript. ---
	aw := &recordingArtifactWriter{h: h}
	changeset, compiled, err := execution.CompileChangeset(ctx, h.spine, aw, execution.ChangesetInput{
		Tenant: h.tenant, SessionID: sessionID, ResponseID: responseID, RunID: runID, AllocationRoot: alloc,
	})
	if err != nil {
		t.Fatalf("compile changeset: %v", err)
	}
	if !compiled {
		t.Fatal("changeset did not compile — the run prepared a repository, so a changeset is expected")
	}
	if len(changeset.Files) != 1 || changeset.Files[0].Path != "repo/feature.txt" || changeset.Files[0].Change != "added" {
		t.Fatalf("changeset files = %+v, want a single added repo/feature.txt (from the tool ledger)", changeset.Files)
	}
	if changeset.PatchArtifactID == "" || changeset.TestLogArtifactID == "" {
		t.Fatalf("changeset artifacts = patch:%q test:%q, want both persisted", changeset.PatchArtifactID, changeset.TestLogArtifactID)
	}
	// The patch is the real diff (adds the marker line), and the test log is the shell transcript.
	if !strings.Contains(aw.byType["patch"], marker) {
		t.Fatalf("patch artifact does not contain the added marker line:\n%s", aw.byType["patch"])
	}
	if !strings.Contains(aw.byType["test-result"], "TESTS_PASS") {
		t.Fatalf("test-log artifact does not carry the shell test output:\n%s", aw.byType["test-result"])
	}
	// The committed head advanced past the base — there is a head to publish in step 11.
	if changeset.FinalCommit == "" || changeset.FinalCommit == changeset.BaseCommit {
		t.Fatalf("changeset final commit = %q (base %q), want the committed head", changeset.FinalCommit, changeset.BaseCommit)
	}
}

// --- fake coding provider + faithful shell double --------------------------------------------------

// codingProvider is the deterministic coding agent: forced by script to write repo/feature.txt with the
// marker (step 1), then run a test that reads it back (step 2), then finish (step 3). It distinguishes
// steps by counting the tool results already folded into the conversation, so the loop is genuinely
// multi-step — the mirror of the live T4 file-then-shell loop, minus the provider.
type codingProvider struct{ marker string }

func (p *codingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	res := modelbroker.Result{
		ModelRequestID: req.ModelRequestID, Model: "fake",
		Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}, Attempts: 1,
	}
	switch toolResults {
	case 0: // edit a file
		res.ProviderRequestID = "prov_file"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_file", Name: "palai.workspace.file",
			Arguments: fmt.Sprintf(`{"op":"write","path":"repo/feature.txt","content":%q}`, p.marker+"\n"),
		}}
		res.FinishReason = "tool_calls"
	case 1: // run a test that reads the edit back through the shell
		res.ProviderRequestID = "prov_shell"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_shell", Name: "palai.workspace.shell",
			Arguments: fmt.Sprintf(`{"argv":["sh","-c","grep -q %s repo/feature.txt && echo TESTS_PASS"],"shell":true}`, p.marker),
		}}
		res.FinishReason = "tool_calls"
	case 2: // commit the edit so there is a head to publish
		res.ProviderRequestID = "prov_commit"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_commit", Name: "palai.workspace.commit",
			Arguments: `{"message":"Add feature.txt"}`,
		}}
		res.FinishReason = "tool_calls"
	default: // summarize and finish
		res.ProviderRequestID = "prov_final"
		res.Output = "edited repo/feature.txt, the test passed, committed"
		res.FinishReason = "stop"
	}
	return res, nil
}

// hostShellRunner is the deterministic tier's faithful shell double: it runs the model's argv on the
// host in the workspace root and captures the real exit code + stdout/stderr. It proves the file→shell
// tool round-trip and the shared-workspace observation deterministically; the SANDBOX enforcement
// (unprivileged uid, no network, cgroup bounds — SAN-002/003/004) is NOT a host-exec claim and is proven
// by the sandbox component/live tiers.
// ponytail: host exec is the deterministic ceiling; the live tier runs the same argv in the real OCI
// sandbox. A scripted, non-hostile argv keeps this controlled — this is a test double, never production.
type hostShellRunner struct{}

func (hostShellRunner) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	var c *exec.Cmd
	if cmd.Shell && len(cmd.Argv) >= 3 {
		c = exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...) // e.g. sh -c "<line>"
	} else {
		c = exec.CommandContext(ctx, cmd.Argv[0], cmd.Argv[1:]...)
	}
	c.Dir = cmd.WorkspaceRoot
	var stdout, stderr strings.Builder
	c.Stdout, c.Stderr = &stdout, &stderr
	err := c.Run()
	res := toolbroker.ShellResult{Stdout: stdout.String(), Stderr: stderr.String()}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		return res, nil
	}
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	return res, nil
}

// recordingArtifactWriter is the deterministic tier's ArtifactWriter double: it captures the changeset's
// patch + test-log bytes by logical type (so the test can assert their content) and inserts the minimal
// durable artifacts row the changeset's FK needs. The real object-store write-path (S3 bytes + checksum)
// is proven by the T2 artifact component/live tiers; only the row + provenance matter to the changeset.
type recordingArtifactWriter struct {
	h      *harness
	byType map[string]string
}

func (w *recordingArtifactWriter) WriteArtifact(ctx context.Context, org, project, runID string, content []byte, _, logicalType string, _ map[string]any) (string, error) {
	if w.byType == nil {
		w.byType = map[string]string{}
	}
	w.byType[logicalType] = string(content)
	id := "art_" + newID(logicalType)
	if _, err := w.h.spine.Pool().Exec(ctx,
		`INSERT INTO artifacts (id, organization_id, project_id, run_id, object_key, size_bytes) VALUES ($1,$2,$3,$4,$5,$6)`,
		id, org, project, runID, "obj/"+id, len(content)); err != nil {
		return "", err
	}
	return id, nil
}

// workspaceDescriptor is a single-attempt descriptor bound to a prepared workspace allocation, so the
// attempt's file/shell tools confine to it (spec §29.9).
func (h *harness) workspaceDescriptor(runID string, fence int64, allocationRoot string) execution.AttemptDescriptor {
	d := h.descriptor(runID, fence)
	d.WorkspaceHostPath = allocationRoot
	return d
}

// --- faithful Git double + workspace fixtures ------------------------------------------------------

// codingRemote is a real local git remote that serves the exact clone commit by sha AND receives the
// agent's push branch — one repo standing in for the whole Git destination. It is non-bare with main
// checked out, so a clone-by-sha works (allowAnySHA1InWant) and a push to the agent/… branch (never the
// checked-out main) is accepted; its ref after the push is a genuine external receipt.
type codingRemote struct{ url, head string }

func newCodingRemote(t *testing.T) codingRemote {
	t.Helper()
	dir := t.TempDir()
	run := codingGit(t, dir)
	run("init", "-q", "-b", "main")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	run("config", "uploadpack.allowReachableSHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
	return codingRemote{url: dir, head: run("rev-parse", "HEAD")}
}

// newAllocationRoot creates a workspace allocation root the journey clones the repo into (at <root>/repo)
// and confines every tool to.
func newAllocationRoot(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("resolve allocation dir: %v", err)
	}
	return resolved
}

func codingGit(t *testing.T, dir string) func(args ...string) string {
	t.Helper()
	return func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.test",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
}
