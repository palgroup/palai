package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
)

// fakeRepoStore stands in for *coordinator.Store: it serves one binding and captures the recorded
// receipt, so the preparation composition is provable without a database (the ReconcileStore pattern).
type fakeRepoStore struct {
	binding  contracts.RepositoryBinding
	found    bool
	recorded *coordinator.PreparationReceiptInput
}

func (f *fakeRepoStore) GetRepositoryBinding(_ context.Context, _ coordinator.Tenant, id string) (contracts.RepositoryBinding, bool, error) {
	if !f.found || string(f.binding.ID) != id {
		return contracts.RepositoryBinding{}, false, nil
	}
	return f.binding, true, nil
}

func (f *fakeRepoStore) RecordPreparationReceipt(_ context.Context, _ coordinator.Tenant, in coordinator.PreparationReceiptInput) error {
	f.recorded = &in
	return nil
}

// TestPrepareRepositoryResolvesRunsRecords proves the run-start preparation step composes the pieces
// (spec §30.3): it resolves the binding, clones the exact commit, and records the model-independent
// receipt keyed to the run. A missing binding fails closed.
func TestPrepareRepositoryResolvesRunsRecords(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	ctx := context.Background()
	remote := newGitRemote(t)

	store := &fakeRepoStore{found: true, binding: contracts.RepositoryBinding{
		ID: "repo_abc", CloneUrl: remote.url, DefaultBranch: "main",
	}}
	tenant := coordinator.Tenant{Organization: "org_x", Project: "prj_x"}

	prep, err := PrepareRepository(ctx, store, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
		BindingID:    "repo_abc",
		RunID:        "run_y",
		RequestedRef: remote.head,
		WorkBranch:   "agent/ses_x/run_y",
		TargetDir:    filepath.Join(t.TempDir(), "repo"),
		SecretsDir:   t.TempDir(),
		AttemptFence: 1,
		ToolCall:     "tcall_z",
	})
	if err != nil {
		t.Fatalf("PrepareRepository() error = %v", err)
	}
	if prep.Receipt.BaseCommit != remote.head {
		t.Fatalf("receipt base commit = %q, want %q", prep.Receipt.BaseCommit, remote.head)
	}
	// The receipt was recorded, keyed to the run, with the exact provenance.
	if store.recorded == nil {
		t.Fatal("PrepareRepository did not record a receipt")
	}
	if store.recorded.RunID != "run_y" || store.recorded.BaseCommit != remote.head || store.recorded.Branch != "agent/ses_x/run_y" {
		t.Fatalf("recorded receipt = %+v, want run_y / %s / agent/ses_x/run_y", *store.recorded, remote.head)
	}

	// A missing binding fails closed — no clone, no receipt.
	store.found = false
	if _, err := PrepareRepository(ctx, store, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
		BindingID: "repo_missing", TargetDir: filepath.Join(t.TempDir(), "repo"), SecretsDir: t.TempDir(),
	}); err == nil {
		t.Fatal("PrepareRepository with a missing binding returned nil error, want fail-closed")
	}
}

type gitRemote struct{ url, head string }

func newGitRemote(t *testing.T) gitRemote {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e.test", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e.test")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q", "-b", "main")
	run("config", "uploadpack.allowAnySHA1InWant", "true")
	run("config", "uploadpack.allowReachableSHA1InWant", "true")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("hello world\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "initial commit")
	return gitRemote{url: dir, head: run("rev-parse", "HEAD")}
}
