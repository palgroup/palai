package execution

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/repositories"
	"github.com/palgroup/palai/adapters/sandboxes/oci/workspace"
	"github.com/palgroup/palai/packages/coordinator"
)

// fakePublicationPump is the store seam faked for the pump test (the ReconcileStore idiom): it serves a
// fixed approved set and records what the pump published / warned, so the boundary-pump is provable with
// a REAL bare remote and no database.
type fakePublicationPump struct {
	approved  []coordinator.Publication
	published map[string]map[string]any
	warned    map[string]string
	// expired names publication ids whose one-shot approval has elapsed: the consume-time guard reports
	// them expired (and records that it was asked), so the pump skips the publish (E10 T7).
	expired    map[string]bool
	expireSeen []string
}

func newFakePump(approved ...coordinator.Publication) *fakePublicationPump {
	return &fakePublicationPump{approved: approved, published: map[string]map[string]any{}, warned: map[string]string{}, expired: map[string]bool{}}
}

func (f *fakePublicationPump) ApprovedPublicationsForRun(context.Context, coordinator.Tenant, string) ([]coordinator.Publication, error) {
	return f.approved, nil
}

func (f *fakePublicationPump) MarkPublicationPublished(_ context.Context, _ coordinator.Tenant, _, _, pubID, _ string, receipt map[string]any) error {
	f.published[pubID] = receipt
	return nil
}

func (f *fakePublicationPump) RecordPublicationWarning(_ context.Context, _ coordinator.Tenant, _, _, pubID, detail string) error {
	f.warned[pubID] = detail
	return nil
}

func (f *fakePublicationPump) ExpireApprovalIfElapsed(_ context.Context, _ coordinator.Tenant, _, _, pubID string) (bool, error) {
	f.expireSeen = append(f.expireSeen, pubID)
	return f.expired[pubID], nil
}

// TestApprovalPumpSkipsExpiredApproval proves the pump-side consume-time expiry guard (spec §22.4, E10
// T7): an approved publication whose one-shot approval elapsed is checked, reported expired, and NOT
// published — an expired approval never pushes. A concurrent non-expired approved publication in the same
// drain publishes unchanged, so the guard is per-row, not a blanket skip.
func TestApprovalPumpSkipsExpiredApproval(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)
	bare := seedBareRemote(t)

	live := coordinator.Publication{
		ID: "pub_live", RunID: "run_1", Operation: "push_branch",
		Remote: bare, Branch: "agent/ses/live", HeadSHA: head, State: "approved",
	}
	stale := coordinator.Publication{
		ID: "pub_stale", RunID: "run_1", Operation: "push_branch",
		Remote: bare, Branch: "agent/ses/stale", HeadSHA: head, State: "approved",
	}
	pump := newFakePump(live, stale)
	pump.expired["pub_stale"] = true // its approval elapsed between approval and this boundary
	publisher := &RepositoryPublisher{Broker: repositories.NewLocalBroker()}

	if err := publishApproved(ctx, pump, publisher, coordinator.Tenant{Organization: "org", Project: "prj"}, "run_1", "ses_1", "resp_1", root, 7); err != nil {
		t.Fatalf("publishApproved() error = %v", err)
	}
	// The expired one was checked but never published, and its branch never reached the remote.
	if _, published := pump.published["pub_stale"]; published {
		t.Fatal("an expired approval must not be published (E10 T7 consume-time guard)")
	}
	if _, err := repoRef(bare, "agent/ses/stale"); err == nil {
		t.Fatal("the expired publication's branch reached the remote; want no push")
	}
	// The live one published exactly as before — the guard is per-row.
	if _, published := pump.published["pub_live"]; !published {
		t.Fatalf("a live approval must still publish; expireSeen=%v warned=%v", pump.expireSeen, pump.warned)
	}
}

// repoRef reports whether a branch exists on a bare remote, and its sha if so.
func repoRef(remote, branch string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "refs/heads/"+branch)
	cmd.Dir = remote
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// TestApprovalPumpPublishesApprovedPushToRemote proves APV-001's publish half deterministically: the
// boundary pump drives an APPROVED push publication through the REAL RepositoryPublisher + publish.go to
// a local bare remote, records the external receipt, and the remote ref ends at the approved head. This
// is the pump end (approve->approved is the coordinator gate; here approved->published).
func TestApprovalPumpPublishesApprovedPushToRemote(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()

	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)
	bare := seedBareRemote(t)
	pub := coordinator.Publication{
		ID: "pub_1", RunID: "run_1", Operation: "push_branch",
		Remote: bare, Branch: "agent/ses/run", HeadSHA: head, State: "approved",
	}
	pump := newFakePump(pub)
	publisher := &RepositoryPublisher{Broker: repositories.NewLocalBroker()}

	tenant := coordinator.Tenant{Organization: "org", Project: "prj"}
	if err := publishApproved(ctx, pump, publisher, tenant, "run_1", "ses_1", "resp_1", root, 7); err != nil {
		t.Fatalf("publishApproved() error = %v", err)
	}
	// The pump recorded the external receipt, and the remote ref really points at the approved head.
	receipt, ok := pump.published["pub_1"]
	if !ok {
		t.Fatalf("pump did not mark the publication published; warned=%v", pump.warned)
	}
	if receipt["remote_sha"] != head {
		t.Fatalf("receipt remote_sha = %v, want the approved head %q", receipt["remote_sha"], head)
	}
	if got := remoteBranch(t, bare, "agent/ses/run"); got != head {
		t.Fatalf("remote ref = %q, want the approved head %q (external receipt)", got, head)
	}
}

// TestApprovalPumpOpensApprovedPullRequest proves the PR branch of the pump: an approved
// open_pull_request publication is published through a fake PullRequestClient, and the pump records the
// PR receipt.
func TestApprovalPumpOpensApprovedPullRequest(t *testing.T) {
	ctx := context.Background()
	pub := coordinator.Publication{
		ID: "pub_pr", RunID: "run_1", Operation: "open_pull_request",
		Remote: "git@h:o/r", Branch: "agent/ses/run", Base: "main", State: "approved",
	}
	pump := newFakePump(pub)
	publisher := &RepositoryPublisher{Broker: repositories.NewLocalBroker(), PRClient: &stubPRClient{}}

	if err := publishApproved(ctx, pump, publisher, coordinator.Tenant{Organization: "org", Project: "prj"}, "run_1", "ses_1", "resp_1", "", 1); err != nil {
		t.Fatalf("publishApproved(PR) error = %v", err)
	}
	receipt, ok := pump.published["pub_pr"]
	if !ok {
		t.Fatalf("pump did not publish the PR; warned=%v", pump.warned)
	}
	if receipt["pull_request_id"] != "PR_9" {
		t.Fatalf("PR receipt id = %v, want PR_9", receipt["pull_request_id"])
	}
}

// TestApprovalPumpWarnsOnPublishFailure proves the REP-010 visibility path: a publish that fails (a
// protected-branch push) leaves the row unpublished and journals a VISIBLE warning rather than silently
// looping.
func TestApprovalPumpWarnsOnPublishFailure(t *testing.T) {
	requireGitExec(t)
	ctx := context.Background()
	root := t.TempDir()
	head := seedWorkspaceRepo(t, root)
	pub := coordinator.Publication{
		ID: "pub_bad", RunID: "run_1", Operation: "push_branch",
		Remote: seedBareRemote(t), Branch: "main", HeadSHA: head, State: "approved", // main is protected -> denied
	}
	pump := newFakePump(pub)
	publisher := &RepositoryPublisher{Broker: repositories.NewLocalBroker()}

	if err := publishApproved(ctx, pump, publisher, coordinator.Tenant{Organization: "org", Project: "prj"}, "run_1", "ses_1", "resp_1", root, 1); err != nil {
		t.Fatalf("publishApproved() error = %v, want a warning not a fatal error", err)
	}
	if _, published := pump.published["pub_bad"]; published {
		t.Fatal("a protected-branch push must not be marked published")
	}
	if _, warned := pump.warned["pub_bad"]; !warned {
		t.Fatal("a failed publish must journal a visible warning (REP-010)")
	}
}

// stubPRClient is a deterministic PullRequestClient for the pump PR test: it opens one PR.
type stubPRClient struct{}

func (stubPRClient) Find(context.Context, string, string) (repositories.PullRequest, bool, error) {
	return repositories.PullRequest{}, false, nil
}
func (stubPRClient) Open(_ context.Context, in repositories.OpenPRInput) (repositories.PullRequest, error) {
	return repositories.PullRequest{ID: "PR_9", URL: "https://example.test/pr/9", Number: 9, Draft: in.Draft}, nil
}

// --- git fixtures ---------------------------------------------------------------------------------

func requireGitExec(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
}

// seedWorkspaceRepo builds a workspace allocation root with a repo/ subdir holding one commit, and
// returns the head sha (the pump reads the repo at root/repo, spec §29.9).
func seedWorkspaceRepo(t *testing.T, root string) string {
	t.Helper()
	repoDir := filepath.Join(root, workspace.RepoDir)
	run := gitAt(t, repoDir)
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	run("init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(repoDir, "f.txt"), []byte("work\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "f.txt")
	run("commit", "-q", "-m", "work")
	return run("rev-parse", "HEAD")
}

func seedBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitAt(t, dir)("init", "-q", "--bare", "-b", "main")
	return dir
}

func remoteBranch(t *testing.T, remote, branch string) string {
	t.Helper()
	return gitAt(t, remote)("rev-parse", "refs/heads/"+branch)
}

func gitAt(t *testing.T, dir string) func(args ...string) string {
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
