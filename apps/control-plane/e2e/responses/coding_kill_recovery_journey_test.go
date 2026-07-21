//go:build e2e

package responses

// TestCodingJourneyWithKillRecoveryDeterministic is the E10 Task 9 deterministic half of §63.2 steps 8-9:
// the coding spine (steps 1-3 + 10-11) with a REAL engine SIGKILL injected at a side-effect boundary
// (step 8) and a ladder RESTORE (step 9) between them, proving the exit-gate sentence deterministically in
// CI — "recovery evidence complete (a §26.12 RecoveryProof), no duplicate tool/push/PR". It EXTENDS the
// E09 coding journey (TestCodingJourneyDeterministic): the same real orchestrator + tools + composed steps
// against the faithful Git double, now with a kill+recovery in the middle, driven by the T4/T5 recovery
// ladder seam (checkpoint sink + killableDialer) this package already owns.
//
// The kill lands AFTER the push tool boundary's checkpoint is durable and the push pending-publication is
// recorded, so the recovery must resume PAST the completed push without re-running it — the "no duplicate
// external effect" core. §63.2 step 7 (research child) is orthogonal to kill+recovery and proven by
// DET-001/002 + the E09 journey; this test omits it to keep the recovery invariant in focus (ponytail).

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/apps/control-plane/internal/execution/tools"
	"github.com/palgroup/palai/packages/coordinator"
	"github.com/palgroup/palai/packages/coordinator/recovery"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/tests/uat"
)

// codingKillProvider drives the same forced coding flow as codingProvider (file -> shell -> commit ->
// push -> PR -> final) but, the FIRST time it reaches the PR step (four tool results folded — i.e. the
// push tool has completed and its boundary checkpoint is durable), it SIGKILLs the live engine and
// returns an error. attempt-1 dies with the push pending-publication already recorded; attempt-2, restored
// past the completed push via the ladder, resumes at the PR step and finishes — the push is never re-run.
type codingKillProvider struct {
	inner   *codingProvider
	kill    func()
	mu      sync.Mutex
	crashed bool
}

func (p *codingKillProvider) Execute(ctx context.Context, req modelbroker.Request, s string, d func(modelbroker.Delta)) (modelbroker.Result, error) {
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	if toolResults == 4 { // push has folded (its checkpoint is durable); about to request the PR
		p.mu.Lock()
		first := !p.crashed
		p.crashed = true
		p.mu.Unlock()
		if first {
			p.kill() // SIGKILL the live engine AFTER the push boundary checkpoint is persisted
			return modelbroker.Result{}, errRecoveryCrash
		}
	}
	return p.inner.Execute(ctx, req, s, d)
}

func TestCodingJourneyWithKillRecoveryDeterministic(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	// --- Steps 1-2: bind the faithful remote + prepare the workspace at the exact commit (E09 spine). ---
	remote := newCodingRemote(t)
	bindingID := newID("bnd")
	if err := h.spine.CreateRepositoryBinding(ctx, h.tenant, coordinator.RepositoryBindingInput{
		BindingID: bindingID, Provider: "local", RepositoryIdentity: "acme/widgets",
		CloneURL: remote.url, DefaultBranch: "main", ConnectionRef: "conn_local",
		AllowedOperations: []string{"push_branch", "open_pull_request"},
	}); err != nil {
		t.Fatalf("create repository binding: %v", err)
	}
	responseID, sessionID, runID := h.admit()

	alloc := newAllocationRoot(t)
	workBranch := "agent/" + sessionID + "/" + runID
	prepared, err := execution.PrepareRepository(ctx, h.spine, repositories.NewLocalBroker(), h.tenant, execution.PrepareRepositoryInput{
		BindingID: bindingID, RunID: runID, RequestedRef: remote.head, WorkBranch: workBranch,
		TargetDir: filepath.Join(alloc, "repo"), SecretsDir: filepath.Join(alloc, "secrets"),
		AttemptFence: 1, ToolCall: "prepare",
	})
	if err != nil {
		t.Fatalf("prepare repository: %v", err)
	}
	if prepared.Receipt.BaseCommit != remote.head {
		t.Fatalf("preparation base commit = %q, want %q", prepared.Receipt.BaseCommit, remote.head)
	}

	// --- Step 3 + 8: run the coding tool loop with a checkpoint sink + a killable real engine; the
	// provider SIGKILLs the engine at the push boundary after its checkpoint is durable. ---
	const marker = "CODING-KILL-RECOVERY-DET-7d2b19"
	store := newMemCheckpointStore()
	dialer := &killableDialer{inner: subprocessDialer{engineDir: h.engineDir}}
	provider := &codingKillProvider{inner: &codingProvider{marker: marker}, kill: dialer.killLatest}
	orch := h.newOrchestratorWithTools(dialer, provider, tools.FileTool(), tools.ShellTool(),
		tools.CommitTool(), tools.TaskTool(), tools.PushTool(), tools.PullRequestTool())
	orch.SetShellRunner(hostShellRunner{})
	orch.SetCheckpointSink(h.checkpointSink(store))

	// attempt-1: reaches the push boundary (checkpoint durable, push pending recorded), then SIGKILL.
	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 1, alloc)); err == nil {
		t.Fatal("attempt-1 must fail after the engine is SIGKILLed at the push boundary")
	}
	if dialer.killCount() == 0 {
		t.Fatal("attempt-1 failed WITHOUT the SIGKILL firing: the recovery would not exercise a real kill")
	}
	if store.objectCount() == 0 {
		t.Fatal("no checkpoint persisted before the kill: the ladder has nothing to restore")
	}
	// The push tool_call completed on attempt-1: its pending publication is already recorded.
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND operation='push_branch'`, runID); n != 1 {
		t.Fatalf("push publications after attempt-1 = %d, want 1 (the push completed before the kill)", n)
	}

	// --- Step 9: attempt-2 restores from the checkpoint via the ladder and finishes — the completed push
	// is NOT re-run (no duplicate external effect), and a complete §26.12 RecoveryProof is journaled. ---
	if err := orch.ExecuteAttempt(ctx, h.workspaceDescriptor(runID, 2, alloc)); err != nil {
		t.Fatalf("attempt-2 (ladder restore after kill) error = %v", err)
	}
	if st, _ := h.response(responseID); st != "completed" {
		t.Fatalf("coding run state after kill+restore = %q, want completed", st)
	}
	if !contains(h.recoveryEventLevels(sessionID), string(recovery.LevelCompatibleCheckpoint)) {
		t.Fatalf("no compatible_checkpoint rung after the kill; levels = %v", h.recoveryEventLevels(sessionID))
	}
	proof, ok := h.recoveryProof(sessionID)
	if !ok || !proof.Complete() {
		t.Fatalf("recovery proof missing/incomplete after kill+restore: %+v (ok=%v)", proof, ok)
	}
	// The file tool (and the push tool) ran exactly ONCE across both attempts — a completed tool is not
	// replayed after a kill+restore. This is the "no duplicate tool" half of the exit-gate sentence.
	if n := h.count(`SELECT count(*) FROM tool_calls WHERE run_id=$1 AND name='palai.workspace.file'`, runID); n != 1 {
		t.Fatalf("file tool_call rows = %d, want 1 (a completed tool must not replay after a kill)", n)
	}
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND operation='push_branch'`, runID); n != 1 {
		t.Fatalf("push publications after restore = %d, want 1 (the completed push must not be re-requested)", n)
	}
	// The checkpoint bytes carry no credential — the §26.2 secret-absence scan extended to CHECKPOINT
	// objects (the snapshot half is SAN-005). The run's own push token is absent from every stored object.
	for _, obj := range store.objects() {
		if bytes.Contains(obj, []byte(detPushSecret)) {
			t.Fatal("a checkpoint object leaked the run's push credential (checkpoint-byte secret scan)")
		}
	}

	// --- Step 10: compile the changeset from the tool ledger (the file write survived the kill). ---
	aw := &recordingArtifactWriter{h: h}
	changeset, compiled, err := execution.CompileChangeset(ctx, h.spine, aw, execution.ChangesetInput{
		Tenant: h.tenant, SessionID: sessionID, ResponseID: responseID, RunID: runID, AllocationRoot: alloc,
	})
	if err != nil {
		t.Fatalf("compile changeset: %v", err)
	}
	if !compiled || len(changeset.Files) != 1 || changeset.Files[0].Path != "repo/feature.txt" {
		t.Fatalf("changeset = %+v (compiled %v), want a single added repo/feature.txt from the tool ledger", changeset.Files, compiled)
	}

	// --- Step 11: approve + publish the push + PR exactly once, despite the mid-run kill. ---
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='pending_approval'`, runID); n != 2 {
		t.Fatalf("pending publications = %d, want 2 (push + PR, once each despite the kill)", n)
	}
	if _, err := h.spine.Pool().Exec(ctx,
		`UPDATE publications SET state='approved' WHERE run_id=$1 AND state='pending_approval'`, runID); err != nil {
		t.Fatalf("approve publications: %v", err)
	}
	publisher := &execution.RepositoryPublisher{Broker: repositories.NewLocalBrokerWithToken(detPushSecret), PRClient: &countingPRClient{}}
	pump := func() {
		approved, err := h.spine.ApprovedPublicationsForRun(ctx, h.tenant, runID)
		if err != nil {
			t.Fatalf("read approved publications: %v", err)
		}
		for _, pub := range approved {
			receipt, err := publisher.Publish(ctx, execution.PublishTarget{
				Publication: pub, WorkspaceRoot: alloc,
				Org: h.tenant.Organization, Project: h.tenant.Project, AttemptFence: 2,
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
	if got := remote.branchSHA(t, workBranch); got != changeset.FinalCommit {
		t.Fatalf("remote ref = %q, want the approved head %q (external receipt)", got, changeset.FinalCommit)
	}
	// Idempotency under recovery: a re-driven pump (E10's detached/recovered execution) republishes NOTHING.
	pump()
	if n := h.count(`SELECT count(*) FROM publications WHERE run_id=$1 AND state='published'`, runID); n != 2 {
		t.Fatalf("published rows after re-drive = %d, want exactly 2 (push + PR, once each)", n)
	}
	if pr := publisher.PRClient.(*countingPRClient); pr.opens != 1 {
		t.Fatalf("PR client opened %d PRs, want exactly 1 (a kill + re-drive must not open a second)", pr.opens)
	}

	// --- Evidence: write + self-verify a recovery bundle. The recovered-run case carries the run's REAL
	// §26.12 RecoveryProof (recovery evidence complete), and the push case carries the genuine external
	// receipt landed exactly once (duplicate external effect = 0). The run's own push token is a needle. ---
	h.writeAndVerifyRecoveryEvidence(t, proof, runID, changeset.FinalCommit, workBranch)
}

// writeAndVerifyRecoveryEvidence builds a recovery-0.1.0-shaped manifest from the recovered run's real
// RecoveryProof + the push external receipt and verifies it clean through the shared verifier (the §26.12
// rule active, 0 secret findings including the run's own push token). It writes to a TEMP dir, not the
// tracked release path — the manifest carries a fresh run_id/proof every run, so writing the tracked file
// would dirty the tree; the tracked recovery-0.1.0 snapshot is the committed deterministic bundle.
func (h *harness) writeAndVerifyRecoveryEvidence(t *testing.T, proof recovery.RecoveryProof, runID, pushSHA, workBranch string) {
	t.Helper()
	root := strings.TrimSpace(mustGit(t, "rev-parse", "--show-toplevel"))
	manifest := map[string]any{
		"release": "recovery-0.1.0", "api_version": "v1",
		"git_sha":     strings.TrimSpace(mustGit(t, "-C", root, "rev-parse", "--short", "HEAD")),
		"migration":   latestMigrationName(t, root),
		"captured_at": time.Now().UTC().Format(time.RFC3339),
		"cases": []any{
			map[string]any{
				"id": "ENG-004", "status": "PASS", "proof_class": "e2e-deterministic",
				"run_id": runID, "image_digest": "sha256:" + strings.Repeat("a", 64),
				"provider_request_id": "prov_final", "mtls_enroll": "runner-local cn=controller",
				"terminal": map[string]any{"type": "response.completed", "count": 1},
				"usage":    map[string]int{"input_tokens": 5, "output_tokens": 3, "total_tokens": 8},
				"db_assertions": []string{
					"a real SIGKILL at the push boundary recovered via the compatible_checkpoint ladder; the completed push was not re-run",
					"every stored checkpoint object was scanned for the run's push token and carried no credential (checkpoint-byte secret scan)",
				},
				"checksum":       hashCoding(runID, "recovery"),
				"recovery_claim": "continued", "recovery_proof": proof,
			},
			map[string]any{
				"id": "REP-006", "status": "PASS", "proof_class": "external-receipt",
				"run_id": runID, "external_receipt": pushSHA,
				"db_assertions": []string{
					"the approved push landed the agent branch " + workBranch + " at exactly the committed head despite the mid-run kill",
					"a re-driven pump republished nothing — no duplicate push, no second PR (duplicate external effect = 0)",
				},
				"checksum": hashCoding(runID, pushSHA, "push-once"),
			},
		},
	}
	dir := t.TempDir()
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatalf("marshal recovery manifest: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), append(raw, '\n'), 0o644); err != nil {
		t.Fatalf("write recovery manifest: %v", err)
	}
	summary, err := uat.VerifyRelease(dir, []string{detPushSecret})
	if err != nil {
		t.Fatalf("verify recovery bundle: %v", err)
	}
	if !summary.OK() || summary.SecretFindings != 0 {
		t.Fatalf("recovery-0.1.0 evidence did not verify clean: %v", summary.Findings)
	}
	t.Logf("evidence (recovery-0.1.0): %s", summary.String())
}
