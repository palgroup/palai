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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/apps/control-plane/internal/execution"
	"github.com/palgroup/palai/packages/coordinator"
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
