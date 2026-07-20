//go:build component

package repository

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
)

// TestApprovedPushExactCommitsOnceTokenDestroyed proves REP-006 against a REAL local bare remote
// (external-receipt): an approved push writes the EXACT approved commit to the branch, once, and the
// brokered push credential is destroyed after the operation (spec §30.9). The remote ref is the
// receipt; the credential-helper file is gone.
func TestApprovedPushExactCommitsOnceTokenDestroyed(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	work, head := newWorkRepoWithCommit(t)
	remote := newBareRemote(t)
	secrets := t.TempDir()

	receipt, err := repositories.PushBranch(ctx, repositories.NewLocalBrokerWithToken("palai-REPMARK-push-secret"), repositories.PushRequest{
		Remote: remote, RepoDir: work, Branch: "agent/ses/run", HeadSHA: head, SecretsDir: secrets,
	})
	if err != nil {
		t.Fatalf("PushBranch() error = %v", err)
	}
	if receipt.Reconciled {
		t.Fatal("a first push must not report reconciled (no prior remote ref)")
	}
	// External receipt: the remote branch now points at exactly the approved commit.
	if got := remoteBranchSHA(t, remote, "agent/ses/run"); got != head {
		t.Fatalf("remote ref = %q, want the approved head %q (external receipt)", got, head)
	}
	if receipt.RemoteSHA != head {
		t.Fatalf("receipt remote sha = %q, want %q", receipt.RemoteSHA, head)
	}
	// The push credential material is destroyed after the operation (spec §30.2): no helper file remains.
	if leftover := credentialFiles(t, secrets); len(leftover) != 0 {
		t.Fatalf("credential helper files remain after push: %v (token not destroyed)", leftover)
	}
}

// TestLostPushAckReconcilesNoDuplicateForce proves REP-007 (external-receipt + fault-live): after a
// push whose ack was lost, re-driving the SAME push reconciles against the remote ref instead of
// pushing again — no duplicate, no force. The retry reports reconciled and the remote is unchanged.
func TestLostPushAckReconcilesNoDuplicateForce(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	work, head := newWorkRepoWithCommit(t)
	remote := newBareRemote(t)
	broker := repositories.NewLocalBrokerWithToken("palai-REPMARK-push-secret")

	req := repositories.PushRequest{Remote: remote, RepoDir: work, Branch: "agent/ses/run", HeadSHA: head, SecretsDir: t.TempDir()}
	if _, err := repositories.PushBranch(ctx, broker, req); err != nil {
		t.Fatalf("first PushBranch() error = %v", err)
	}
	// The ack was lost: the caller re-drives the exact same operation (a retry, or E10 detached
	// execution). The remote already holds the head, so reconciliation short-circuits — no second push.
	req.SecretsDir = t.TempDir()
	receipt, err := repositories.PushBranch(ctx, broker, req)
	if err != nil {
		t.Fatalf("reconciling PushBranch() error = %v", err)
	}
	if !receipt.Reconciled {
		t.Fatal("a re-driven push whose remote is already at the head must report reconciled (no duplicate)")
	}
	if got := remoteBranchSHA(t, remote, "agent/ses/run"); got != head {
		t.Fatalf("remote ref after reconcile = %q, want the unchanged head %q", got, head)
	}
}

// TestBaseMovementNoDroppedRemoteChanges proves REP-010 (external-receipt): when the remote branch has
// moved to a commit our push is not a fast-forward of, the push is REFUSED (ErrRemoteDiverged), never
// forced — the remote change survives, it is not silently dropped (spec §30.12).
func TestBaseMovementNoDroppedRemoteChanges(t *testing.T) {
	requireGit(t)
	ctx := context.Background()

	work, head := newWorkRepoWithCommit(t)
	remote := newBareRemote(t)
	broker := repositories.NewLocalBrokerWithToken("palai-REPMARK-push-secret")

	// First push lands the agent branch at head.
	if _, err := repositories.PushBranch(ctx, broker, repositories.PushRequest{
		Remote: remote, RepoDir: work, Branch: "agent/ses/run", HeadSHA: head, SecretsDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("initial PushBranch() error = %v", err)
	}
	// Someone else advances the remote branch out-of-band (the remote moved).
	moved := advanceRemoteBranch(t, remote, "agent/ses/run", head)

	// A fresh local commit on top of the OLD head diverges from the moved remote. Pushing it is not a
	// fast-forward, so it must be refused — not forced.
	diverged := commitOnto(t, work, head, "diverge.txt", "local divergent work")
	_, err := repositories.PushBranch(ctx, broker, repositories.PushRequest{
		Remote: remote, RepoDir: work, Branch: "agent/ses/run", HeadSHA: diverged, SecretsDir: t.TempDir(),
	})
	if !errors.Is(err, repositories.ErrRemoteDiverged) {
		t.Fatalf("diverged push error = %v, want ErrRemoteDiverged (never a force)", err)
	}
	// The remote change was NOT dropped: the branch still holds the out-of-band commit.
	if got := remoteBranchSHA(t, remote, "agent/ses/run"); got != moved {
		t.Fatalf("remote ref = %q, want the preserved out-of-band commit %q (a force silently dropped it)", got, moved)
	}
}

// --- fixtures -------------------------------------------------------------------------------------

// newWorkRepoWithCommit builds a non-bare working repo with one commit and returns its dir + head sha.
func newWorkRepoWithCommit(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	run := gitRunner(t, dir)
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README.md")
	run("commit", "-q", "-m", "work commit")
	return dir, run("rev-parse", "HEAD")
}

// newBareRemote builds an empty bare repo that serves as a push destination.
func newBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitRunner(t, dir)("init", "-q", "--bare", "-b", "main")
	return dir
}

// remoteBranchSHA reads the sha a bare remote's branch points at (the external receipt).
func remoteBranchSHA(t *testing.T, remote, branch string) string {
	t.Helper()
	return gitRunner(t, remote)("rev-parse", "refs/heads/"+branch)
}

// advanceRemoteBranch adds a commit to a bare remote's branch out-of-band (a second clone pushes it),
// simulating the remote moving after our first push, and returns the new sha.
func advanceRemoteBranch(t *testing.T, remote, branch, base string) string {
	t.Helper()
	clone := t.TempDir()
	run := gitRunner(t, clone)
	run("clone", "-q", remote, ".")
	run("checkout", "-q", "-B", branch, base)
	if err := os.WriteFile(filepath.Join(clone, "remote-side.txt"), []byte("advanced on the remote\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "remote-side.txt")
	run("commit", "-q", "-m", "remote-side advance")
	run("push", "-q", "origin", "HEAD:refs/heads/"+branch)
	return remoteBranchSHA(t, remote, branch)
}

// commitOnto checks out base in repoDir, adds a file, commits, and returns the new sha.
func commitOnto(t *testing.T, repoDir, base, name, content string) string {
	t.Helper()
	run := gitRunner(t, repoDir)
	run("checkout", "-q", "-B", "local-work", base)
	if err := os.WriteFile(filepath.Join(repoDir, name), []byte(content+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", name)
	run("commit", "-q", "-m", "local "+name)
	return run("rev-parse", "HEAD")
}

// credentialFiles lists any git-credentials helper files left in dir — none should remain after a push.
func credentialFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read secrets dir: %v", err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "git-credentials-") {
			out = append(out, e.Name())
		}
	}
	return out
}
