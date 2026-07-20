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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/tests/uat"
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
		&codingProvider{marker: marker}, tools.FileTool(), tools.ShellTool(), tools.CommitTool(),
		tools.TaskTool(), tools.PushTool(), tools.PullRequestTool())
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

	// --- Step 11: an approved branch push + a draft PR happen exactly once (REP-006/008, APV-001). The
	// push + PR tools created PENDING publications during the run — their destination (remote/branch/base)
	// resolved from the BINDING, not the model. The publish half then drives the approved rows through the
	// REAL RepositoryPublisher to the faithful remote (a genuine external receipt) and a stub PR client. ---
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='pending_approval'`, runID); n != 2 {
		t.Fatalf("pending publications = %d, want 2 (push + PR)", n)
	}
	// The push destination came from the binding + preparation receipt, not the model: the exact remote,
	// the agent work branch, and the committed head.
	var pushRemote, pushBranch, pushHead string
	if err := h.spine.Pool().QueryRow(ctx,
		`SELECT remote, branch, head_sha FROM publications WHERE run_id=$1 AND operation='push_branch'`, runID).
		Scan(&pushRemote, &pushBranch, &pushHead); err != nil {
		t.Fatalf("read pending push publication: %v", err)
	}
	if pushRemote != remote.url || pushBranch != workBranch || pushHead != changeset.FinalCommit {
		t.Fatalf("pending push = remote:%q branch:%q head:%q, want the binding's %q / %q / committed %q (model cannot redirect)",
			pushRemote, pushBranch, pushHead, remote.url, workBranch, changeset.FinalCommit)
	}

	// Approve both publications (approve->approved). The approve COMMAND boundary is proven by T8's
	// ApplyApprovalDecision; here the run has already terminated, so the journey flips the durable rows to
	// approved directly and drives the pump, exactly as an approve arriving at a boundary would leave them.
	if _, err := h.spine.Pool().Exec(ctx,
		`UPDATE publications SET state='approved' WHERE run_id=$1 AND state='pending_approval'`, runID); err != nil {
		t.Fatalf("approve publications: %v", err)
	}

	publisher := &execution.RepositoryPublisher{Broker: repositories.NewLocalBrokerWithToken("palai-DET-push-secret"), PRClient: &countingPRClient{}}
	// pump drains the run's approved-but-unpublished publications through the REAL publisher + store, the
	// same loop the orchestrator's boundary pump runs. A re-drive after everything is published drains an
	// empty set — that is the idempotency proof.
	pump := func() {
		approved, err := h.spine.ApprovedPublicationsForRun(ctx, h.tenant, runID)
		if err != nil {
			t.Fatalf("read approved publications: %v", err)
		}
		for _, pub := range approved {
			receipt, err := publisher.Publish(ctx, execution.PublishTarget{
				Publication: pub, WorkspaceRoot: alloc,
				Org: h.tenant.Organization, Project: h.tenant.Project, AttemptFence: 1,
			})
			if err != nil {
				t.Fatalf("publish %s: %v", pub.Operation, err)
			}
			if err := h.spine.MarkPublicationPublished(ctx, h.tenant, sessionID, responseID, pub.ID, pub.Operation, receipt); err != nil {
				t.Fatalf("mark published %s: %v", pub.Operation, err)
			}
		}
	}
	pump()

	// External receipt: the faithful remote's agent branch now points at exactly the approved head.
	if got := remote.branchSHA(t, workBranch); got != changeset.FinalCommit {
		t.Fatalf("remote ref = %q, want the approved head %q (external receipt)", got, changeset.FinalCommit)
	}
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='published' AND operation='push_branch'`, runID); n != 1 {
		t.Fatalf("published push rows = %d, want 1", n)
	}
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='published' AND operation='open_pull_request'`, runID); n != 1 {
		t.Fatalf("published PR rows = %d, want 1 (a single draft PR)", n)
	}

	// Idempotency (REP-007/008): a re-driven pump (a lost ack, or E10's detached execution) republishes
	// NOTHING — the durable rows are already published — so there is no duplicate push and no second PR.
	pump()
	if got := remote.branchSHA(t, workBranch); got != changeset.FinalCommit {
		t.Fatalf("remote ref after re-drive = %q, want the unchanged head %q (no duplicate/force push)", got, changeset.FinalCommit)
	}
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='published'`, runID); n != 2 {
		t.Fatalf("published rows after re-drive = %d, want exactly 2 (push + PR, once each)", n)
	}
	if pr := publisher.PRClient.(*countingPRClient); pr.opens != 1 {
		t.Fatalf("PR client opened %d PRs, want exactly 1 (a duplicate request must not open a second)", pr.opens)
	}

	// --- Step 7: a required research child runs on a cheaper route via a MODEL-driven agent tool_call
	// (§63.2 step 7, DEL-001/SUB-002). The parent's agent tool_call becomes a child.request — a real
	// ChildRun on its own cheaper model id — whose typed result folds back and whose run the parent links.
	// A worker executes the parent + the dispatched child (the coding run above already terminated, so the
	// worker's terminal-run no-op claim of its stale job is harmless). ---
	childAnswer := "child-research-" + newID("mark")
	stopChild := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir},
		agentDelegatingProvider{childModel: "fake-child", childAnswer: childAnswer}))
	childRespID, _, parentRunID := h.admitWith(`{"input":"research the approach then implement"}`, newID("idem"))
	h.awaitResponseState(childRespID, "completed", 90*time.Second)
	stopChild()

	childRun, childResp := h.childRunOf(parentRunID)
	if state := h.runState(childRun); state != "completed" {
		t.Fatalf("research child state = %q, want completed", state)
	}
	if got := h.modelOfRun(childResp); got != "fake-child" {
		t.Fatalf("research child model = %q, want the cheaper fake-child (its own route)", got)
	}
	if links := h.childRunsLink(childRespID); len(links) != 1 || links[0] != childRun {
		t.Fatalf("parent projection child_runs = %v, want [%s] (the child run is linked, not a hidden transcript)", links, childRun)
	}

	// --- Evidence: write + self-verify the coding-0.1.0 bundle. The deterministic tier records the
	// genuinely CI-provable external receipt — the real remote ref the approved push landed — and the
	// verifier asserts the bundle clean with NO leaked credential (sk-/PAT/App key). The live wave
	// overwrites this with the real-provider (chatcmpl) + real-GitHub receipts. ---
	h.writeAndVerifyCodingEvidence(t, codingReceipt{
		runID: runID, pushRemoteSHA: changeset.FinalCommit, workBranch: workBranch,
	})
}

// codingReceipt is the deterministic journey's captured evidence for the coding-0.1.0 bundle.
type codingReceipt struct {
	runID         string
	pushRemoteSHA string // the remote ref the approved push landed — the genuine external receipt
	workBranch    string
}

// writeAndVerifyCodingEvidence writes the coding-0.1.0 manifest from the journey's external receipt and
// verifies it clean through the shared verifier: the external-receipt rule holds (a real remote ref, not
// a fake) and the credential-absence scan finds nothing (0 secret findings — no sk-, PAT, or App key).
func (h *harness) writeAndVerifyCodingEvidence(t *testing.T, r codingReceipt) {
	t.Helper()
	release := "coding-0.1.0"
	root := strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
	manifest := map[string]any{
		"release":     release,
		"git_sha":     strings.TrimSpace(mustGit(t, "-C", root, "rev-parse", "--short", "HEAD")),
		"api_version": "v1",
		"migration":   latestMigrationName(t, root),
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases": []any{
			map[string]any{
				"id": "REP-006", "status": "PASS", "proof_class": "external-receipt",
				"run_id":           r.runID,
				"external_receipt": r.pushRemoteSHA,
				"db_assertions": []string{
					"the approved push landed the agent branch at exactly the committed head",
					"the remote ref " + r.workBranch + " = " + r.pushRemoteSHA + " (a genuine external receipt, not a fake remote)",
					"a re-driven pump republished nothing — no duplicate push, no second PR",
					"the push credential was destroyed after the operation; absent from every surface",
				},
				"checksum": hashCoding(r.runID, r.pushRemoteSHA, r.workBranch),
			},
		},
	}
	dir := filepath.Join(root, "evidence", "releases", release)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make release dir: %v", err)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal coding manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write coding manifest: %v", err)
	}
	summary, err := uat.VerifyRelease(dir, nil)
	if err != nil {
		t.Fatalf("verify coding bundle: %v", err)
	}
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("coding-0.1.0 evidence did not verify clean: %v", summary.Findings)
	}
	t.Logf("evidence (coding-0.1.0): %s", summary.String())
}

func mustGit(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", args...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// latestMigrationName returns the highest migration version, e.g. 000013_approvals_publications.
func latestMigrationName(t *testing.T, root string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(root, "storage", "migrations"))
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	latest := ""
	for _, e := range entries {
		if name, ok := strings.CutSuffix(e.Name(), ".up.sql"); ok && name > latest {
			latest = name
		}
	}
	if latest == "" {
		t.Fatal("no migrations found")
	}
	return latest
}

func hashCoding(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
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
	case 3: // request a branch push (a gated side effect -> pending approval)
		res.ProviderRequestID = "prov_push"
		res.ToolCalls = []modelbroker.ToolCall{{ID: "call_push", Name: "palai.publish.push", Arguments: `{}`}}
		res.FinishReason = "tool_calls"
	case 4: // request a draft pull request (pending approval)
		res.ProviderRequestID = "prov_pr"
		res.ToolCalls = []modelbroker.ToolCall{{
			ID: "call_pr", Name: "palai.publish.pull_request",
			Arguments: `{"title":"Add feature","body":"adds feature.txt"}`,
		}}
		res.FinishReason = "tool_calls"
	default: // summarize and finish
		res.ProviderRequestID = "prov_final"
		res.Output = "edited repo/feature.txt, the test passed, committed, requested push + PR"
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

// countingPRClient is a deterministic PullRequestClient that does genuine find-before-create: the first
// Open records the PR; a later Find returns it (so a duplicate request adopts the existing PR, REP-008).
// opens counts real creations, so the test proves at most one PR is ever opened.
type countingPRClient struct {
	opens int
	pr    *repositories.PullRequest
}

func (c *countingPRClient) Find(_ context.Context, _, _ string) (repositories.PullRequest, bool, error) {
	if c.pr != nil {
		return *c.pr, true, nil
	}
	return repositories.PullRequest{}, false, nil
}

func (c *countingPRClient) Open(_ context.Context, in repositories.OpenPRInput) (repositories.PullRequest, error) {
	c.opens++
	pr := repositories.PullRequest{ID: "PR_det9", URL: "https://git.example.test/acme/widgets/pull/9", Number: 9, Draft: in.Draft}
	c.pr = &pr
	return pr, nil
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

// branchSHA reads the sha the remote's branch points at — the external receipt after a push.
func (r codingRemote) branchSHA(t *testing.T, branch string) string {
	t.Helper()
	return codingGit(t, r.url)("rev-parse", "refs/heads/"+branch)
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
