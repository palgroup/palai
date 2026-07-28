//go:build live

// Package repository_test is the E09 Task 3 live repository-clone smoke. It runs only under the
// `live` build tag, in `make test-live-provider PROVIDER=provider-one CASE=repository-clone`, which
// loads credentials from the git-ignored .env.local. It clones a REAL Git repository at an exact
// commit through the infrastructure-owned preparation and proves the brokered Git credential is
// absent from every surface — the live confirmation of the REP-003 exit-gate invariant.
//
// HONEST CEILING: this is the LIVE half of REP-003. The deterministic tier
// (tests/security/repository) already proves credential absence with a fake token — an absence proof
// needs no provider-realness. This tier confirms it with a REAL credential: when the GitHub App
// env is present it mints a REAL installation token (installation token > user PAT, §30.2) and clones
// a private repo; otherwise it clones PALAI_GIT_REPO with the local broker. Because a minted token is
// opaque (the broker never reveals it), absence is proven by SHAPE — no gh[pso]_ token and no
// credential-in-URL appears on any captured surface — plus the clean fetch URL and the removed helper.
package repository_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/repositories"
)

// credentialShapes match a real Git credential on any captured surface: a GitHub token (gh[pso]_...)
// or a credential embedded in a URL (user:secret@host / x-access-token:...@). None may appear.
var credentialShapes = []*regexp.Regexp{
	regexp.MustCompile(`gh[psou]_[A-Za-z0-9]{20,}`),
	regexp.MustCompile(`x-access-token:[^@\s]+@`),
	regexp.MustCompile(`://[^/@\s:]+:[^@\s/]+@`),
}

func TestLiveRepositoryCloneCredentialAbsent(t *testing.T) {
	repoURL := os.Getenv("PALAI_GIT_REPO")
	if repoURL == "" {
		t.Skip("PALAI_GIT_REPO is required for the live repository-clone smoke")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ref := os.Getenv("PALAI_GIT_COMMIT")
	if ref == "" {
		ref = os.Getenv("PALAI_GIT_REF")
	}
	ctx := context.Background()

	broker, tier := liveBroker(t, repoURL)
	t.Logf("live repository-clone tier: %s", tier)

	target := t.TempDir()
	secrets := t.TempDir()
	trace := installGitTrace(t)

	res, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL:      repoURL,
		RequestedRef:  ref,
		DefaultBranch: envOr("PALAI_GIT_DEFAULT_BRANCH", "main"),
		TargetDir:     filepath.Join(target, "repo"),
		SecretsDir:    secrets,
		WorkBranch:    "agent/live_ses/live_run",
		Audience: repositories.Audience{
			Organization: "org_live", Project: "prj_live", Run: "run_live", AttemptFence: 1, ToolCall: "tcall_live",
		},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if res.Receipt.BaseCommit == "" || res.Receipt.TreeHash == "" {
		t.Fatalf("receipt is missing exact-commit provenance: %+v", res.Receipt)
	}
	if ref != "" && !strings.HasPrefix(res.Receipt.BaseCommit, strings.TrimSpace(ref)) && res.Receipt.BaseCommit != ref {
		// A full-SHA ref must resolve to that exact commit; a branch/tag ref is not asserted here.
		if len(ref) == 40 {
			t.Fatalf("receipt base commit = %q, want the exact requested commit %q", res.Receipt.BaseCommit, ref)
		}
	}

	// Surface scan: no credential shape on the git trace (argv/env/remote URL), the receipt, or the
	// engine-visible worktree — the REAL token is absent by shape.
	traceBytes, err := os.ReadFile(trace)
	if err != nil {
		t.Fatalf("read git trace: %v", err)
	}
	if !bytes.Contains(traceBytes, []byte("fetch")) {
		t.Fatal("git trace captured no fetch — the shim did not intercept preparation")
	}
	scanForCredentialShape(t, "git trace (argv/env/remote URL)", traceBytes)
	// The fetch URL argv must be exactly the clean repo URL — no credential spliced in.
	if !bytes.Contains(traceBytes, []byte(repoURL)) {
		t.Fatalf("git trace does not show the clean repo URL %q as fetched", repoURL)
	}
	receiptJSON, _ := json.Marshal(res.Receipt)
	scanForCredentialShape(t, "preparation receipt", receiptJSON)
	scanWorktree(t, filepath.Join(target, "repo"))

	// The read credential's helper file was removed after preparation (§30.3 step 9).
	_ = filepath.WalkDir(secrets, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "git-credentials-") {
			t.Fatalf("credential helper file survived preparation: %s", path)
		}
		return nil
	})
}

// liveBroker builds the credential broker for the run: a real GitHub App installation-token minter
// when the App env is present (the strongest tier), else the local broker for a public/local repo.
func liveBroker(t *testing.T, repoURL string) (repositories.Broker, string) {
	t.Helper()
	appID := os.Getenv("PALAI_GITHUB_APP_ID")
	installID := os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID")
	keyFile := os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE")
	if appID != "" && installID != "" && keyFile != "" {
		pem, err := os.ReadFile(keyFile)
		if err != nil {
			t.Fatalf("read GitHub App private key: %v", err)
		}
		var repos []string
		if r := os.Getenv("PALAI_GITHUB_APP_REPO"); r != "" {
			repos = []string{r}
		}
		broker, err := repositories.NewGitHubAppBroker(repositories.GitHubAppConfig{
			AppID: appID, InstallationID: installID, PrivateKeyPEM: pem, Repositories: repos,
		})
		if err != nil {
			t.Fatalf("NewGitHubAppBroker() error = %v", err)
		}
		return broker, "github-app-installation-token (real)"
	}
	return repositories.NewLocalBroker(), "local-broker (PALAI_GIT_REPO, no App env)"
}

func scanForCredentialShape(t *testing.T, surface string, data []byte) {
	t.Helper()
	for _, re := range credentialShapes {
		if re.Match(data) {
			t.Fatalf("a Git credential shape (%s) leaked into %s", re.String(), surface)
		}
	}
}

func scanWorktree(t *testing.T, root string) {
	t.Helper()
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.Contains(path, string(os.PathSeparator)+".git"+string(os.PathSeparator)) {
			return nil // git's own object store is not a model-visible surface
		}
		if body, rerr := os.ReadFile(path); rerr == nil {
			scanForCredentialShape(t, "worktree file "+path, body)
		}
		return nil
	})
}

func installGitTrace(t *testing.T) string {
	t.Helper()
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not found: %v", err)
	}
	binDir := t.TempDir()
	trace := filepath.Join(t.TempDir(), "git-trace.log")
	shim := "#!/bin/sh\n{ echo \"ARGV: $*\"; env; } >> " + shellQuote(trace) + "\nexec " + shellQuote(realGit) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(shim), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return trace
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// ============================================================================
// E22 T3 leg: the repository a SLACK THREAD was bound to, cloned for real.
// ============================================================================
//
// WHAT THIS ADDS TO THE FILE ABOVE, and it is one decision rather than one more clone: a Slack
// connection's default_policy carries repository_binding_id and — optionally — repository_ref, and T3
// leaves the ref EMPTY on purpose. Empty falls back to the binding's default_branch (Request.DefaultBranch
// here), which is ALSO the base a publication targets (RunPublicationTarget reads rb.default_branch). One
// value, so the branch the agent edits and the branch its pull request targets cannot disagree.
//
// That fallback is asserted against a REAL remote, because it is the kind of claim a fake remote agrees
// with whatever it does: this is where PALAI_GIT_BASE_BRANCH is proven to be the branch actually checked
// out. It runs no control plane and no Slack — it proves the seam under the connection's policy.
func TestLiveSlackBoundRepositoryClonesAtItsBaseBranch(t *testing.T) {
	cloneURL := os.Getenv("PALAI_GIT_CLONE_URL")
	if cloneURL == "" {
		t.Skip("PALAI_GIT_CLONE_URL is required for the live Slack-bound repository leg (E22 §0.2)")
	}
	base := os.Getenv("PALAI_GIT_BASE_BRANCH")
	if base == "" {
		t.Skip("PALAI_GIT_BASE_BRANCH is required for the live Slack-bound repository leg (E22 §0.2)")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	ctx := context.Background()
	broker, tier := liveBroker(t, cloneURL)
	t.Logf("live Slack-bound repository tier: %s (base branch %s)", tier, base)

	target := t.TempDir()
	repoDir := filepath.Join(target, "repo")
	res, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL: cloneURL,
		// EMPTY — the whole point. A Slack connection that names no repository_ref must land on the
		// binding's default_branch, which is what §0.2's PALAI_GIT_BASE_BRANCH becomes.
		RequestedRef:  "",
		DefaultBranch: base,
		TargetDir:     repoDir,
		SecretsDir:    t.TempDir(),
		WorkBranch:    "agent/live_ses_e22t3/live_run_e22t3",
		Audience: repositories.Audience{
			Organization: "org_live", Project: "prj_live", Run: "run_live_e22t3", AttemptFence: 1, ToolCall: "provision",
		},
	})
	if err != nil {
		t.Fatalf("Prepare() with an EMPTY requested ref error = %v — a Slack connection that names no "+
			"repository_ref must still clone", err)
	}
	if res.Receipt.RequestedRef != "" {
		t.Fatalf("receipt.requested_ref = %q, want empty: the receipt must record that NO ref was asked for, "+
			"or the provenance claims a choice nobody made", res.Receipt.RequestedRef)
	}
	// The exact commit the empty ref resolved to must BE the base branch's tip on the remote. Read from the
	// remote rather than from the local clone: the local checkout is what we are testing, so believing it
	// would make the assertion circular.
	tip := gitOutput(t, target, "ls-remote", cloneURL, "refs/heads/"+base)
	if tip == "" {
		t.Fatalf("the remote has no refs/heads/%s: PALAI_GIT_BASE_BRANCH names a branch that does not exist, "+
			"so every pull request from this workspace would target nothing", base)
	}
	if head := strings.Fields(tip)[0]; head != res.Receipt.BaseCommit {
		t.Fatalf("an empty requested ref resolved to %s but refs/heads/%s is %s: the branch the agent edits and "+
			"the branch its pull request targets have diverged, which is exactly what leaving the ref empty "+
			"exists to prevent", res.Receipt.BaseCommit, base, head)
	}
	// NO WORKTREE CREDENTIAL SCAN HERE, and the omission is measured rather than lazy: scanWorktree greps
	// the CLONED SOURCE for credential SHAPES, which is sound for REP-003's fixed repository and a false
	// positive generator for an arbitrary customer one. Run against this very repository on 2026-07-28 it
	// failed on adapters/integrations/a2a/pusher_test.go, whose fixture contains a `://user:pass@host` URL.
	// The credential-absence claim is TestLiveRepositoryCloneCredentialAbsent's and is not weakened by
	// leaving it there; this leg's claim is the BRANCH, and a test that fails on the contents of the
	// customer's repository proves nothing about either.
	if _, err := os.Stat(filepath.Join(repoDir, ".git")); err != nil {
		t.Fatalf("no .git in the prepared tree at %s: %v", repoDir, err)
	}
}

// gitOutput runs git in dir and returns its trimmed stdout, failing the test on error.
func gitOutput(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var out, errBuf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errBuf
	if err := cmd.Run(); err != nil {
		t.Fatalf("git %s: %v (%s)", strings.Join(args, " "), err, errBuf.String())
	}
	return strings.TrimSpace(out.String())
}

// ============================================================================
// E22 T4 leg: a REAL push and a REAL DRAFT pull request, through a REAL GitHub App.
// ============================================================================
//
// WHAT ONLY A REAL PROVIDER CAN ANSWER, and it is why this leg exists at all: whether the pull request
// GitHub actually created is a DRAFT. Every deterministic tier around it agrees that it is — the fake PR
// client in the coding journey returns `in.Draft`, and OpenPullRequest sets `in.Draft = true` a line before
// calling it (publish.go:233) — so a fake can only ever echo our own belief back. Draft is a state on
// GitHub's side, and this is the one test that reads it from there.
//
// It also closes the destination claim at its far end: the branch appears on the real remote at exactly the
// approved head, and the pull request's base is the branch §0.2's PALAI_GIT_BASE_BRANCH named.
//
// THE CEILING, stated because the shapes are so close: this drives repositories.PushBranch +
// OpenPullRequest — the BODY of execution.RepositoryPublisher.Publish — rather than the publisher type
// itself, because Go's internal rule forbids tests/ importing apps/control-plane/internal. What the
// publisher adds on top (the operation switch, the per-publish temp secrets dir, the receipt shape) is
// proven against a real spine by the component tier. What is NOT proven anywhere is a merge, a review
// request or a CI wait: E22 opens a draft and stops.
//
// IT WRITES TO A REAL REPOSITORY. The branch is unique per run (nothing is overwritten) and the pull
// request is a draft against PALAI_GIT_BASE_BRANCH — never main unless the operator pointed the base
// there. The branch and the PR are LEFT BEHIND on purpose: deleting them would need delete scopes this
// App is not asked for, and a test that quietly deletes a pull request is worse than one that leaves a
// visible artifact named after itself.
func TestLiveApprovedPushAndDraftPullRequest(t *testing.T) {
	for _, name := range []string{"PALAI_GITHUB_APP_ID", "PALAI_GITHUB_APP_INSTALLATION_ID",
		"PALAI_GITHUB_APP_PRIVATE_KEY_FILE", "PALAI_GIT_CLONE_URL", "PALAI_GIT_BASE_BRANCH", "PALAI_GIT_REPO"} {
		if os.Getenv(name) == "" {
			t.Skipf("%s is required for the live push + draft-PR leg (E22 §0.2)", name)
		}
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git not found: %v", err)
	}
	cloneURL, base := os.Getenv("PALAI_GIT_CLONE_URL"), os.Getenv("PALAI_GIT_BASE_BRANCH")
	owner, repo, ok := strings.Cut(os.Getenv("PALAI_GIT_REPO"), "/")
	if !ok || owner == "" || repo == "" {
		t.Skip("PALAI_GIT_REPO must be owner/repo for the live push + draft-PR leg")
	}
	ctx := context.Background()

	keyPEM, err := os.ReadFile(os.Getenv("PALAI_GITHUB_APP_PRIVATE_KEY_FILE"))
	if err != nil {
		t.Fatalf("read GitHub App private key: %v", err)
	}
	cfg := repositories.GitHubAppConfig{
		AppID:          os.Getenv("PALAI_GITHUB_APP_ID"),
		InstallationID: os.Getenv("PALAI_GITHUB_APP_INSTALLATION_ID"),
		PrivateKeyPEM:  keyPEM,
		Repositories:   []string{repo},
	}
	broker, err := repositories.NewGitHubAppBroker(cfg)
	if err != nil {
		t.Fatalf("NewGitHubAppBroker() error = %v", err)
	}

	// 1. Prepare the workspace the way a run does: clone at the binding's base branch onto a work branch.
	run := "run_e22t4_" + strconv.FormatInt(time.Now().UnixNano(), 36)
	branch := "agent/live_e22t4/" + run
	target := t.TempDir()
	repoDir := filepath.Join(target, "repo")
	audience := repositories.Audience{
		Organization: "org_live", Project: "prj_live", Run: run, AttemptFence: 1, ToolCall: "publish",
	}
	if _, err := repositories.Prepare(ctx, broker, repositories.Request{
		CloneURL: cloneURL, RequestedRef: "", DefaultBranch: base, TargetDir: repoDir,
		SecretsDir: t.TempDir(), WorkBranch: branch, Audience: audience,
	}); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}

	// 2. A real commit on the work branch — the head an approver would have approved.
	marker := "palai live E22 T4 " + run + "\n"
	if err := os.WriteFile(filepath.Join(repoDir, "PALAI_LIVE_E22T4.md"), []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	gitOutput(t, repoDir, "add", "PALAI_LIVE_E22T4.md")
	commit := exec.Command("git", "commit", "-q", "-m", "palai live E22 T4 "+run)
	commit.Dir = repoDir
	commit.Env = append(os.Environ(), "GIT_AUTHOR_NAME=palai-live", "GIT_AUTHOR_EMAIL=palai-live@example.test",
		"GIT_COMMITTER_NAME=palai-live", "GIT_COMMITTER_EMAIL=palai-live@example.test")
	if out, err := commit.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v: %s", err, out)
	}
	head := gitOutput(t, repoDir, "rev-parse", "HEAD")

	// 3. The push — the side effect an Approve authorizes. Its receipt is the remote ref itself.
	secrets := t.TempDir()
	receipt, err := repositories.PushBranch(ctx, broker, repositories.PushRequest{
		Remote: cloneURL, RepoDir: repoDir, Branch: branch, HeadSHA: head,
		SecretsDir: secrets, Audience: audience,
	})
	if err != nil {
		t.Fatalf("PushBranch() error = %v", err)
	}
	if receipt.RemoteSHA != head {
		t.Fatalf("push receipt remote sha = %q, want the approved head %q", receipt.RemoteSHA, head)
	}
	// Read the remote back rather than believing the receipt: the receipt is our own code's account of what
	// happened, and what this tier exists for is what the PROVIDER says.
	if tip := gitOutput(t, target, "ls-remote", cloneURL, "refs/heads/"+branch); tip == "" ||
		strings.Fields(tip)[0] != head {
		t.Fatalf("refs/heads/%s on the remote is %q, want the approved head %s", branch, tip, head)
	}
	// The credential the push minted left no file behind (§30.3 step 9) and no shape on the receipt.
	_ = filepath.WalkDir(secrets, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasPrefix(d.Name(), "git-credentials-") {
			t.Fatalf("push credential helper file survived: %s", path)
		}
		return nil
	})
	receiptJSON, _ := json.Marshal(receipt)
	scanForCredentialShape(t, "push receipt", receiptJSON)

	// 4. The pull request. DRAFT is not an argument here — OpenPullRequest sets it, §30.8 policy — and the
	// base is the binding's, never the model's.
	prClient, err := repositories.NewGitHubPullRequestClient(cfg, owner, repo)
	if err != nil {
		t.Fatalf("NewGitHubPullRequestClient() error = %v", err)
	}
	pr, err := repositories.OpenPullRequest(ctx, prClient, repositories.OpenPRInput{
		HeadBranch: branch, Base: base,
		Title:      "Agent changes: " + branch,
		Body:       "open draft pull request " + branch + " -> " + base,
	})
	if err != nil {
		t.Fatalf("OpenPullRequest() error = %v", err)
	}
	if !pr.Draft {
		t.Fatalf("GitHub created pull request %s as a NON-DRAFT (%+v): every deterministic tier around this one "+
			"can only echo our own `in.Draft = true` back, so this is the only place the claim is tested. A "+
			"non-draft PR is a change requesting review the approver did not ask for", pr.URL, pr)
	}
	if pr.URL == "" || pr.Number == 0 {
		t.Fatalf("the opened pull request carries no url/number: %+v", pr)
	}
	t.Logf("live E22 T4: pushed %s @ %s and opened DRAFT pull request #%d (%s) against %s",
		branch, head, pr.Number, pr.URL, base)

	// 5. Idempotency against the real provider (REP-008): a second request for the SAME head->base adopts
	// the existing pull request rather than opening a second one.
	again, err := repositories.OpenPullRequest(ctx, prClient, repositories.OpenPRInput{
		HeadBranch: branch, Base: base, Title: "Agent changes: " + branch, Body: "duplicate request",
	})
	if err != nil {
		t.Fatalf("second OpenPullRequest() error = %v", err)
	}
	if again.Number != pr.Number {
		t.Fatalf("a duplicate request opened pull request #%d beside #%d — one approval, one pull request",
			again.Number, pr.Number)
	}
}
