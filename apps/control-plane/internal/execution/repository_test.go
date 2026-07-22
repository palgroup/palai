package execution

import (
	"context"
	"errors"
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

// TestPrepareRepositoryResolvesBindingConnectionRef proves the E13 T9 seam at the composition level: a
// binding that names a connection_ref resolves its Git credential through the secret-ref resolver under
// the RUN's server-minted organization (never a binding-carried one), a ref-less binding never consults
// the resolver at all (today's global-broker path, unchanged), and a resolver failure fails the
// preparation CLOSED with an error that names the REF and never the value.
func TestPrepareRepositoryResolvesBindingConnectionRef(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found on PATH: %v", err)
	}
	ctx := context.Background()
	remote := newGitRemote(t)
	tenant := coordinator.Tenant{Organization: "org_x", Project: "prj_x"}

	prepare := func(t *testing.T, ref string, resolver SecretResolver) error {
		t.Helper()
		store := &fakeRepoStore{found: true, binding: contracts.RepositoryBinding{
			ID: "repo_abc", CloneUrl: remote.url, DefaultBranch: "main", ConnectionRef: ref,
		}}
		_, err := PrepareRepository(ctx, store, repositories.NewLocalBroker(), tenant, PrepareRepositoryInput{
			BindingID:         "repo_abc",
			RunID:             "run_y",
			RequestedRef:      remote.head,
			WorkBranch:        "agent/ses_x/run_y",
			TargetDir:         filepath.Join(t.TempDir(), "repo"),
			SecretsDir:        t.TempDir(),
			ConnectionSecrets: resolver,
		})
		return err
	}

	// A named ref is resolved under the run's organization and the binding's ref.
	var gotOrg, gotRef string
	calls := 0
	if err := prepare(t, "git-conn", func(org, ref string) ([]byte, error) {
		calls, gotOrg, gotRef = calls+1, org, ref
		return []byte("palai-REPMARK-binding-token"), nil
	}); err != nil {
		t.Fatalf("PrepareRepository(connection_ref) error = %v", err)
	}
	if calls != 1 || gotOrg != tenant.Organization || gotRef != "git-conn" {
		t.Fatalf("resolver calls=%d org=%q ref=%q, want 1 / %q / git-conn", calls, gotOrg, gotRef, tenant.Organization)
	}

	// A ref-less binding takes the global broker unchanged: the resolver is never consulted.
	if err := prepare(t, "", func(org, ref string) ([]byte, error) {
		t.Fatalf("ref-less binding consulted the secret resolver for (%q, %q)", org, ref)
		return nil, nil
	}); err != nil {
		t.Fatalf("PrepareRepository(ref-less) error = %v", err)
	}

	// A ref-bearing binding with NO resolver wired fails CLOSED. This is the latent-trap guard: a
	// composition root that wires the workspace provisioner but forgets the resolver must not silently
	// clone every tenant's binding under the deployment-global credential.
	if err := prepare(t, "git-conn", nil); err == nil {
		t.Fatal("a ref-bearing binding with no resolver wired cloned under the global broker, want fail-closed")
	}

	// A resolver failure fails CLOSED — it must not silently fall back to the deployment-global
	// credential — and the message names the ref, never the value.
	err := prepare(t, "git-conn", func(string, string) ([]byte, error) {
		return []byte("palai-REPMARK-binding-token"), errors.New("secret ref exists but could not be decrypted")
	})
	if err == nil {
		t.Fatal("PrepareRepository with an unresolvable connection_ref returned nil error, want fail-closed")
	}
	if !strings.Contains(err.Error(), "git-conn") || strings.Contains(err.Error(), "palai-REPMARK-binding-token") {
		t.Fatalf("error = %q, want it to name the ref and not the value", err)
	}

	// An empty resolved credential is a misconfiguration, not an anonymous clone.
	if err := prepare(t, "git-conn", func(string, string) ([]byte, error) { return nil, nil }); err == nil {
		t.Fatal("PrepareRepository with an empty resolved credential returned nil error, want fail-closed")
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
