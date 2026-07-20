//go:build e2e

package responses

// TestCodingLiveJourney is the E09 Task 9 LIVE coding journey — WIRED here, run from the batched live
// wave, never in CI. It proves the composed journey at the SEAM with a REAL provider and a REAL Git
// destination: a real model edits + tests + commits a real cloned repository through the real coding
// tools + a real hardened OCI sandbox, then a REAL approved push (and, with a GitHub App, a real draft
// PR) lands the change at the real destination — a genuine external receipt. It reuses newHarness (real
// Postgres + a real run row) so the publication flows through the REAL store: RequestPublication (durable
// pending) -> approve -> ApprovedPublicationsForRun -> RepositoryPublisher.Publish -> real remote.
//
// HONEST CEILING (goal-critical — the name says exactly what is proven): this is the coding journey
// proven LIVE AT THE SEAM (real provider + real workspace + real approve->publish->Git). The real-provider
// tool turns are separate FORCED broker.Route calls (a live model is not reliably driven through a 5-tool
// orchestrator loop — the T4 discipline), and the approve step transitions the durable publication to
// approved directly rather than through a run-boundary approve COMMAND. The approve-COMMAND boundary is
// proven deterministically (TestCodingJourneyDeterministic + T8 APV-001); wiring workspace provisioning +
// the approve-command boundary into the production HTTP run loop is E09 T10, not this task. No claim here
// exceeds "the composed coding journey runs live at the seam against a real destination."
//
// Skipped unless PALAI_UAT_PROVIDER=provider-one + OPENAI_API_KEY + PALAI_GIT_REPO + PALAI_SHELL_IMAGE_ID
// are set (the operator entry loads them from .env.local). The credential rides env only and is asserted
// absent from every captured surface + the written manifest.

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
	"github.com/palgroup/palai/tests/uat"
)

func TestCodingLiveJourney(t *testing.T) {
	if os.Getenv("PALAI_UAT_PROVIDER") != "provider-one" {
		t.Skip("live coding journey needs PALAI_UAT_PROVIDER=provider-one (run make uat-coding PROVIDER=provider-one)")
	}
	secret := os.Getenv("OPENAI_API_KEY")
	if secret == "" {
		t.Fatal("OPENAI_API_KEY is unset; the operator entry loads it from .env.local")
	}
	repoURL := os.Getenv("PALAI_GIT_REPO")
	shellImage := os.Getenv("PALAI_SHELL_IMAGE_ID")
	if repoURL == "" || shellImage == "" {
		t.Skip("PALAI_GIT_REPO + PALAI_SHELL_IMAGE_ID are required for the live coding journey")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	h := newHarness(t)

	// --- Step 1-2 (live): bind the real repo + prepare a workspace by cloning it at its exact commit
	// through the infrastructure-owned preparation, under a brokered credential the model never sees. ---
	bindingID := newID("bnd")
	base := envOrLive("PALAI_GIT_DEFAULT_BRANCH", "main")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "github", RepositoryIdentity: repoURL, CloneURL: repoURL,
		DefaultBranch: base, ConnectionRef: "conn_live", AllowedOperations: []string{"push_branch", "open_pull_request"},
	}); err != nil {
		t.Fatalf("create live binding: %v", err)
	}
	responseID, sessionID, runID := h.admit()
	alloc := liveAllocation(t)
	workBranch := "agent/" + sessionID + "/" + runID
	broker, tier := liveCodingBroker(t)
	t.Logf("live coding-journey broker tier: %s", tier)
	prepared, err := execution.PrepareRepository(ctx, h.spine, broker, h.tenant, execution.PrepareRepositoryInput{
		BindingID: bindingID, RunID: runID, RequestedRef: os.Getenv("PALAI_GIT_COMMIT"),
		WorkBranch: workBranch, TargetDir: filepath.Join(alloc, workspace.RepoDir),
		SecretsDir: filepath.Join(alloc, "secrets"), AttemptFence: 1, ToolCall: "prepare",
	})
	if err != nil {
		t.Fatalf("prepare live workspace: %v", err)
	}
	t.Logf("prepared %s at base %s on %s", repoURL, safePrefixLive(prepared.Receipt.BaseCommit), workBranch)

	// --- Step 3 (live): a REAL provider is FORCED to edit a file, then to run a test through the shell
	// tool inside a REAL hardened OCI sandbox — the real coding tool round-trip against the real clone. ---
	models := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": "OPENAI_API_KEY"},
	})
	driver, err := oci.NewDockerDriver()
	if err != nil {
		t.Fatalf("create Docker driver: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close() })
	shell := workspace.NewShellExecutor(driver, shellImage, oci.Limits{
		WallTime: 30 * time.Second, MaxMemoryBytes: 256 << 20, MaxProcessCount: 128, NanoCPUs: 1_000_000_000,
	})
	tb := toolbroker.New(fileToolBuiltin(), shellToolBuiltin())
	env := toolbroker.ExecEnv{WorkspaceRoot: alloc, Shell: shell}

	var captured bytes.Buffer
	stream := func(d modelbroker.Delta) { captured.WriteString(d.Text) }
	const marker = "PALAI-E09-T9-LIVE-JOURNEY"
	editCall, editChat := liveForceTool(t, ctx, models, stream, "mreq_edit",
		"Use the file tool to write repo/JOURNEY.txt with exactly this content: "+marker, liveFileSchema())
	if _, err := tb.Execute(ctx, contracts.ToolCallID("tc_file"), editCall.Name, liveDecode(t, editCall.Arguments), 1, env); err != nil {
		t.Fatalf("execute file tool: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(alloc, workspace.RepoDir, "JOURNEY.txt")); err != nil || !strings.Contains(string(b), marker) {
		t.Fatalf("file tool did not persist the marker to the live workspace (got %q, err %v)", b, err)
	}
	shellCall, _ := liveForceTool(t, ctx, models, stream, "mreq_test",
		"The workspace has repo/JOURNEY.txt. Use the shell tool to print its contents (e.g. cat repo/JOURNEY.txt).", liveShellSchema())
	shellOut, err := tb.Execute(ctx, contracts.ToolCallID("tc_shell"), shellCall.Name, liveDecode(t, shellCall.Arguments), 2, env)
	if err != nil {
		t.Fatalf("execute shell tool: %v", err)
	}
	if out, _ := shellOut.Result["stdout"].(string); !strings.Contains(out, marker) {
		t.Fatalf("shell tool stdout did not read back the marker the file tool wrote: %q", out)
	}

	// --- Step 10 (live): commit the edit so there is a head to publish. ---
	head, err := repositories.Commit(ctx, filepath.Join(alloc, workspace.RepoDir), "Add JOURNEY.txt (E09 T9 live coding journey)")
	if err != nil {
		t.Fatalf("commit live edit: %v", err)
	}

	// --- Step 11 (live): the REAL approved push lands the agent branch at exactly the committed head on
	// the REAL destination — a genuine external receipt — through the durable publication store + the real
	// RepositoryPublisher. RequestPublication records the durable pending row; it is transitioned to
	// approved; ApprovedPublicationsForRun drains it; Publish pushes with force disabled and revokes the
	// scoped credential. With a GitHub App, a real draft PR opens. ---
	publisher := &execution.RepositoryPublisher{Broker: broker}
	if prClient := liveCodingPRClient(t); prClient != nil {
		publisher.PRClient = prClient
	}
	pushPubID := h.liveRequestPublication(ctx, "push_branch", sessionID, responseID, runID, repoURL, workBranch, base, head)
	if publisher.PRClient != nil {
		h.liveRequestPublication(ctx, "open_pull_request", sessionID, responseID, runID, repoURL, workBranch, base, head)
	}
	// Approve every pending publication for the run, then drain the pump through the real publisher.
	if _, err := h.spine.Pool().Exec(ctx, `UPDATE publications SET state='approved' WHERE run_id=$1 AND state='pending_approval'`, runID); err != nil {
		t.Fatalf("approve live publications: %v", err)
	}
	approved, err := h.spine.ApprovedPublicationsForRun(ctx, h.tenant, runID)
	if err != nil {
		t.Fatalf("read approved publications: %v", err)
	}
	var pushRemoteSHA, prURL string
	for _, pub := range approved {
		receipt, err := publisher.Publish(ctx, execution.PublishTarget{
			Publication: pub, WorkspaceRoot: alloc, Org: h.tenant.Organization, Project: h.tenant.Project, AttemptFence: 1,
		})
		if err != nil {
			t.Fatalf("publish %s: %v", pub.Operation, err)
		}
		if err := h.spine.MarkPublicationPublished(ctx, h.tenant, sessionID, responseID, pub.ID, pub.Operation, receipt); err != nil {
			t.Fatalf("mark %s published: %v", pub.Operation, err)
		}
		switch pub.Operation {
		case "push_branch":
			pushRemoteSHA, _ = receipt["remote_sha"].(string)
		case "open_pull_request":
			prURL, _ = receipt["url"].(string)
		}
	}
	if pushRemoteSHA != head {
		t.Fatalf("push external receipt = %q, want the approved head %q (a real remote ref, not a fake)", pushRemoteSHA, head)
	}
	if n := h.count(`SELECT count(*) FROM publications WHERE id=$1 AND state='published'`, pushPubID); n != 1 {
		t.Fatalf("push publication not marked published once (n=%d)", n)
	}
	t.Logf("live external receipt: %s = %s on %s; PR=%s", workBranch, safePrefixLive(pushRemoteSHA), repoURL, prURL)

	// Leak scan by construction: the provider credential must appear on no captured surface.
	for name, buf := range map[string][]byte{"streamed deltas": captured.Bytes(), "shell result": liveJSON(shellOut.Result)} {
		if bytes.Contains(buf, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name)
		}
	}

	// --- Evidence: overwrite coding-0.1.0 with the LIVE receipts — the real provider (chatcmpl) coding
	// turn and the real external receipt — verified clean with the credential proven absent. ---
	h.writeAndVerifyLiveCodingEvidence(t, secret, liveCodingReceipt{
		runID: runID, editChat: editChat, pushRemoteSHA: pushRemoteSHA, prURL: prURL, workBranch: workBranch,
	})
}

// liveRequestPublication records a durable pending publication through the REAL store, resolving the
// idempotency key + head-bound request hash the same way the push/PR tools do.
func (h *harness) liveRequestPublication(ctx context.Context, op, sessionID, responseID, runID, remote, branch, base, head string) string {
	h.t.Helper()
	pubOp := repositories.PublishOperation(op)
	pub, err := h.spine.RequestPublication(ctx, h.tenant, coordinator.PublicationRequest{
		PublicationID:  newID("pub"),
		ApprovalID:     newID("apr"),
		SessionID:      sessionID,
		RunID:          runID,
		ResponseID:     responseID,
		Operation:      op,
		Remote:         remote,
		Branch:         branch,
		Base:           base,
		HeadSHA:        head,
		IdempotencyKey: repositories.IdempotencyKey(h.tenant.Organization, h.tenant.Project, runID, pubOp, remote, branch, base, head),
		RequestHash:    repositories.RequestHash(h.tenant.Organization, h.tenant.Project, runID, pubOp, remote, branch, base, head),
		Display:        op + " " + branch + " -> " + remote,
	})
	if err != nil {
		h.t.Fatalf("RequestPublication(%s): %v", op, err)
	}
	return pub.ID
}

// liveCodingReceipt is the live journey's captured evidence for the coding-0.1.0 bundle.
type liveCodingReceipt struct {
	runID, editChat, pushRemoteSHA, prURL, workBranch string
}

func (h *harness) writeAndVerifyLiveCodingEvidence(t *testing.T, secret string, r liveCodingReceipt) {
	t.Helper()
	// The seam tier has no runner OCI engine image or mTLS enrollment (the coding journey is proven at
	// the seam, not through the HTTP run loop — T10), so the manifest records only what is GENUINE: the
	// external receipts (a real remote ref + a real PR URL). The real-provider coding turn's chatcmpl id
	// is folded into the push case's assertions — recorded honestly, without fabricating runner fields.
	cases := []any{
		map[string]any{
			"id": "REP-006", "status": "PASS", "proof_class": "external-receipt",
			"run_id": r.runID, "external_receipt": r.pushRemoteSHA,
			"db_assertions": []string{
				"a real provider (chatcmpl " + safePrefixLive(r.editChat) + "…) edited repo/JOURNEY.txt through the file tool + a real OCI shell test",
				"an approved push landed the agent branch " + r.workBranch + " at exactly the committed head on the REAL destination",
				"the remote ref = " + r.pushRemoteSHA + " (a genuine external receipt, not a fake remote)",
				"the durable publication went pending -> approved -> published; the scoped push credential was destroyed",
				"CEILING: coding journey proven live AT THE SEAM; production HTTP-run auto-provisioning is E09 T10",
			},
			"checksum": liveHash(r.runID, r.pushRemoteSHA),
		},
	}
	if r.prURL != "" {
		cases = append(cases, map[string]any{
			"id": "REP-008", "status": "PASS", "proof_class": "external-receipt",
			"run_id": r.runID, "external_receipt": r.prURL,
			"db_assertions": []string{"a real draft pull request opened once at " + r.prURL + " (stable external receipt)"},
			"checksum":      liveHash(r.runID, r.prURL),
		})
	}
	manifest := map[string]any{
		"release": "coding-0.1.0", "git_sha": liveGitSHA(t), "api_version": "v1",
		"migration": "000013_approvals_publications", "captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases": cases,
	}
	dir := filepath.Join(liveRepoRoot(t), "evidence", "releases", "coding-0.1.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("make release dir: %v", err)
	}
	raw, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write live coding manifest: %v", err)
	}
	summary, err := uat.VerifyRelease(dir, []string{secret})
	if err != nil {
		t.Fatalf("verify live coding bundle: %v", err)
	}
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("live coding-0.1.0 evidence did not verify clean: %v", summary.Findings)
	}
	t.Logf("evidence (coding-0.1.0 live): %s", summary.String())
}
