package repositories

import (
	"context"
	"testing"
)

// TestPushToProtectedDefaultBranchDenied proves REP-004 at the publish path: a push targeting a
// protected or default branch is denied unless the policy explicitly grants it — the first mutable
// publication caller of DirectWorkAllowed (spec §30.5/§30.9). A generated agent/<...> branch is allowed.
func TestPushToProtectedDefaultBranchDenied(t *testing.T) {
	for _, branch := range []string{"main", "master", "release"} {
		_, err := PushBranch(context.Background(), NewLocalBroker(), PushRequest{
			Remote: "/tmp/does-not-matter", RepoDir: t.TempDir(), Branch: branch, HeadSHA: "deadbeef",
			Protected: []string{"release"}, SecretsDir: t.TempDir(),
		})
		if err == nil {
			t.Fatalf("PushBranch(branch=%q) = nil error, want protected-branch denial before any git runs", branch)
		}
	}
}

// TestIdempotencyKeyIsOperationSpecific proves decision (b): a push key includes the head SHA (a new
// head is a NEW push — never a silent force-over), while a pull-request key EXCLUDES it (a PR tracks the
// branch across new commits, so a duplicate request dedupes to one PR, REP-008). Keys are run-scoped so
// two runs never collide.
func TestIdempotencyKeyIsOperationSpecific(t *testing.T) {
	push := func(head string) string {
		return IdempotencyKey("org", "prj", "run1", OpPushBranch, "git@h:o/r", "agent/s/r", "main", head)
	}
	if push("aaa") == push("bbb") {
		t.Fatal("push idempotency key must change with the head SHA (a new head is a new push, not a force)")
	}
	pr := func(head string) string {
		return IdempotencyKey("org", "prj", "run1", OpOpenPullRequest, "git@h:o/r", "agent/s/r", "main", head)
	}
	if pr("aaa") != pr("bbb") {
		t.Fatal("pull-request idempotency key must NOT depend on the head SHA (a PR tracks the branch → one PR, REP-008)")
	}
	// Run-scoped: a different run is a different key even for the same branch+head.
	if push("aaa") == IdempotencyKey("org", "prj", "run2", OpPushBranch, "git@h:o/r", "agent/s/r", "main", "aaa") {
		t.Fatal("idempotency key must be run-scoped")
	}
}

// TestRequestHashBindsHeadForApproval proves the one-shot approval binding (spec §22.4, REP-009): the
// request hash ALWAYS includes the head SHA, even for a pull request, so a head that moves after approval
// yields a different request hash — the prior approval no longer matches and the action is denied.
func TestRequestHashBindsHeadForApproval(t *testing.T) {
	at := func(op PublishOperation, head string) string {
		return RequestHash("org", "prj", "run1", op, "git@h:o/r", "agent/s/r", "main", head)
	}
	if at(OpPushBranch, "aaa") == at(OpPushBranch, "bbb") {
		t.Fatal("request hash must bind the head SHA for a push")
	}
	if at(OpOpenPullRequest, "aaa") == at(OpOpenPullRequest, "bbb") {
		t.Fatal("request hash must bind the head SHA even for a pull request (REP-009 stale-approval)")
	}
}

// TestApprovalMatchesHeadRejectsMovedHead proves REP-009's staleness rule: an approval granted for one
// head does not authorize a different (moved) head, and an unknown current head never matches.
func TestApprovalMatchesHeadRejectsMovedHead(t *testing.T) {
	if !ApprovalMatchesHead("abc123", "abc123") {
		t.Fatal("an approval must match its own approved head")
	}
	if ApprovalMatchesHead("abc123", "def456") {
		t.Fatal("a moved head must invalidate the approval (REP-009)")
	}
	if ApprovalMatchesHead("abc123", "") || ApprovalMatchesHead("", "abc123") {
		t.Fatal("an unknown head is never a match")
	}
}

// TestMergeDeniedByDefault proves merge is excluded from the ordinary coding set (spec §30.8, §30.11):
// merge/release/protected-branch push are DENY unless the policy explicitly enables them, so a stale
// approval can never reach a merge in the default posture (REP-009 belt-and-suspenders).
func TestMergeDeniedByDefault(t *testing.T) {
	if MergeAllowed(nil) {
		t.Fatal("merge must be denied by default (excluded from the ordinary coding set, §30.8)")
	}
	if !MergeAllowed([]string{"merge"}) {
		t.Fatal("merge must be allowed only when the policy explicitly grants it")
	}
}

// fakePRClient is a deterministic PullRequestClient: it records opened PRs in memory keyed by
// (headBranch->base), so Find returns an existing PR and a duplicate Open is caught — the REP-008
// find-before-create idempotency provable without a real Git provider.
type fakePRClient struct {
	prs   map[string]PullRequest
	opens int
}

func newFakePRClient() *fakePRClient { return &fakePRClient{prs: map[string]PullRequest{}} }

func (f *fakePRClient) key(head, base string) string { return head + "->" + base }

func (f *fakePRClient) Find(_ context.Context, head, base string) (PullRequest, bool, error) {
	pr, ok := f.prs[f.key(head, base)]
	return pr, ok, nil
}

func (f *fakePRClient) Open(_ context.Context, in OpenPRInput) (PullRequest, error) {
	f.opens++
	pr := PullRequest{ID: "PR_1", URL: "https://example.test/pr/1", Number: 1, Draft: in.Draft}
	f.prs[f.key(in.HeadBranch, in.Base)] = pr
	return pr, nil
}

// TestDuplicatePROpenReturnsSinglePR proves REP-008: OpenPullRequest FINDS an existing open PR for
// (head->base) before creating, so a duplicate request or lost-ack callback returns the SAME PR and
// opens exactly one — and the default policy opens a DRAFT.
func TestDuplicatePROpenReturnsSinglePR(t *testing.T) {
	client := newFakePRClient()
	in := OpenPRInput{HeadBranch: "agent/s/r", Base: "main", Title: "t", Body: "b"}

	first, err := OpenPullRequest(context.Background(), client, in)
	if err != nil {
		t.Fatalf("first OpenPullRequest error = %v", err)
	}
	if !first.Draft {
		t.Fatal("default publication policy must open a DRAFT pull request (§30.8)")
	}
	second, err := OpenPullRequest(context.Background(), client, in)
	if err != nil {
		t.Fatalf("second OpenPullRequest error = %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("duplicate request opened a different PR (%q vs %q); want the same PR (REP-008)", second.ID, first.ID)
	}
	if client.opens != 1 {
		t.Fatalf("PR opens = %d, want exactly 1 (find-before-create idempotency)", client.opens)
	}
}
