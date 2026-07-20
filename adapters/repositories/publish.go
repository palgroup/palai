package repositories

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// This file is the infrastructure side of publication (spec §30.8-30.12): pushing a branch and opening
// a draft pull request, each a SEPARATE operation with its own idempotency key, credential, and remote
// receipt. Like preparation, the model never enters here — the push credential is minted just in time,
// reaches Git only through the store helper file (never argv/URL/log, §30.2), and is revoked on every
// path. Every operation is reconciliation-safe: it queries the remote truth before acting, so a lost
// ack never double-pushes and E10's detached execution re-drives the SAME code with zero rework.

// PublishOperation is one decomposed publication operation (spec §30.8). Each carries its own
// capability, approval, credential, idempotency key, and audit — there is no atomic "push + PR" unit,
// so "branch pushed but PR not opened" is a legitimate intermediate state, not a partial failure.
type PublishOperation string

const (
	OpPushBranch      PublishOperation = "push_branch"
	OpOpenPullRequest PublishOperation = "open_pull_request"
	OpMerge           PublishOperation = "merge"
)

// ErrProtectedBranch is returned when a push targets a protected or default branch without an explicit
// policy grant (spec §30.5/§30.9, REP-004).
var ErrProtectedBranch = errors.New("protected_branch_write_denied")

// ErrRemoteDiverged is returned when the remote branch has moved to a commit our push is not a
// fast-forward of (spec §30.12, REP-010). Force is a separate high-risk capability that is never
// inferred, so the push is refused rather than forced — the remote change is preserved, never silently
// dropped. The caller resolves it explicitly (rebase/merge/wait).
var ErrRemoteDiverged = errors.New("remote_diverged")

// IdempotencyKey is the operation-specific dedupe identity of a publication (spec §30.8-30.10;
// decision (b)). A push includes the head SHA — a new head is a NEW push, never a silent force-over an
// existing ref; a pull request EXCLUDES it — a PR tracks the branch across new commits, so a duplicate
// request dedupes to ONE PR (REP-008). It is tenant + run scoped so one run's publication never
// collides with another's. The UNIQUE column on publications enforces the dedupe at the database.
func IdempotencyKey(org, project, runID string, op PublishOperation, remote, branch, base, headSHA string) string {
	var parts []string
	switch op {
	case OpOpenPullRequest:
		parts = []string{org, project, runID, string(op), remote, branch, base}
	default: // push_branch, merge: the exact head is part of the operation identity
		parts = []string{org, project, runID, string(op), remote, branch, base, headSHA}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "pub_" + hex.EncodeToString(sum[:16])
}

// RequestHash is the one-shot binding of an approval to the EXACT operation it authorizes (spec §22.4,
// REP-009). Unlike the idempotency key it ALWAYS includes the head SHA — even for a pull request — so a
// head that moves after approval yields a different request hash and the prior approval no longer
// matches: a changed head needs a fresh approval, and an edited argument set is a new tool call with a
// new request hash. The approve command carries this hash; the store admits it only against a pending
// approval whose hash equals it.
func RequestHash(org, project, runID string, op PublishOperation, remote, branch, base, headSHA string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{org, project, runID, string(op), remote, branch, base, headSHA}, "\x00")))
	return "req_" + hex.EncodeToString(sum[:16])
}

// ApprovalMatchesHead reports whether an approval granted for approvedHead still authorizes the current
// head (spec §30.11, REP-009): an approval is bound to the exact head it was granted for, so a head
// that moved after approval makes the approval STALE and the action must be denied. An unknown current
// head (empty) is never a match — fail closed.
func ApprovalMatchesHead(approvedHead, currentHead string) bool {
	return approvedHead != "" && approvedHead == currentHead
}

// MergeAllowed reports whether merge is permitted by policy (spec §30.8, §30.11). Merge, release, and
// protected-branch push are EXCLUDED from the ordinary coding set and denied by default; a binding
// enables merge only by listing it in allowedOperations. So a stale approval can never reach a merge in
// the default posture (REP-009 belt-and-suspenders on top of ApprovalMatchesHead).
func MergeAllowed(allowedOperations []string) bool {
	for _, op := range allowedOperations {
		if op == string(OpMerge) {
			return true
		}
	}
	return false
}

// PushRequest is the infrastructure-owned request to push a branch (spec §30.9). It is not
// model-controlled: the remote, branch, and exact head come from the resolved binding + the approved
// publication, so the push publishes exactly the approved content regardless of where the local branch
// HEAD has since moved.
type PushRequest struct {
	Remote     string   // clean remote URL (no credential, §30.2)
	RepoDir    string   // the workspace repo whose object the head SHA lives in
	Branch     string   // the branch to create/advance on the remote (must pass DirectWorkAllowed)
	HeadSHA    string   // the EXACT commit to publish — pushed by sha, not "current HEAD"
	Protected  []string // policy-widened protected set (default main/master always protected)
	SecretsDir string   // the snapshot-excluded /secrets area for the credential helper (§29.10)
	Audience   Audience // binds the minted push credential to this operation (§28.11)
}

// PushReceipt is the external receipt of a push (spec §30.9, REP-006): the remote ref and the sha it
// points at after the operation. Reconciled is true when the remote was ALREADY at the head and no push
// was performed (a lost-ack retry or E10 re-drive — REP-007), so the caller records the same receipt
// without a duplicate push.
type PushReceipt struct {
	Remote     string
	Branch     string
	RemoteSHA  string
	Reconciled bool
}

// PushBranch pushes Branch at exactly HeadSHA to Remote with force DISABLED (spec §30.9, REP-006/007).
// It is idempotent and reconciliation-safe: it first queries the remote ref, and if the remote is
// ALREADY at HeadSHA it records the receipt without pushing (a lost ack or E10 re-drive never
// double-pushes). A push that would not be a fast-forward is refused, not forced (ErrRemoteDiverged,
// REP-010). The credential is minted just in time, reaches Git only via the store helper file, and is
// revoked on every return path (§30.2) — so a late error can never leave a live token behind.
func PushBranch(ctx context.Context, broker Broker, req PushRequest) (PushReceipt, error) {
	if req.Remote == "" || req.RepoDir == "" || req.Branch == "" || req.HeadSHA == "" {
		return PushReceipt{}, fmt.Errorf("push: remote, repo dir, branch, and head are required")
	}
	// REP-004: direct work on a protected/default branch is denied before any git runs or any
	// credential is minted — a protected-branch push is refused, not attempted.
	if !DirectWorkAllowed(req.Branch, req.Protected) {
		return PushReceipt{}, fmt.Errorf("push to %q: %w", req.Branch, ErrProtectedBranch)
	}
	// Untrusted-input guards: a flag-shaped remote/branch/head would be reparsed by git as an option.
	if err := rejectGitPositionals(map[string]string{"remote": req.Remote, "branch": req.Branch, "head": req.HeadSHA}); err != nil {
		return PushReceipt{}, fmt.Errorf("push: %w", err)
	}

	// Mint the write-scoped credential just in time; revoke on EVERY path (spec §30.2).
	cred, err := broker.Mint(ctx, ScopePush, req.Audience)
	if err != nil {
		return PushReceipt{}, fmt.Errorf("push: mint push credential: %w", err)
	}
	defer func() { _ = broker.Revoke(ctx, cred.Handle) }()
	helperConfig, err := broker.writeHelper(cred.Handle, req.Remote, req.SecretsDir)
	if err != nil {
		return PushReceipt{}, fmt.Errorf("push: %w", err)
	}
	// The token reaches git ONLY through the helper file named here — never in argv or the remote URL.
	credConfig := []string{"-c", "credential.helper=" + helperConfig}

	// Reconcile-before-push (spec §30.9, REP-007): if the remote ref is already at the head, the push
	// already happened (a lost ack, or E10 re-driving this exact operation). Record the receipt without
	// pushing — no duplicate, no force.
	remoteSHA, found, err := RemoteRef(ctx, req.RepoDir, req.Remote, req.Branch, credConfig)
	if err != nil {
		return PushReceipt{}, fmt.Errorf("push: query remote ref: %w", err)
	}
	if found && remoteSHA == req.HeadSHA {
		return PushReceipt{Remote: req.Remote, Branch: req.Branch, RemoteSHA: remoteSHA, Reconciled: true}, nil
	}

	// Push EXACTLY the approved commit to the branch, force DISABLED. Pushing by sha (not "HEAD")
	// publishes the approved content even if the local branch has since advanced. Git enforces
	// fast-forward-only without --force, so a diverged remote is rejected rather than overwritten.
	refspec := req.HeadSHA + ":refs/heads/" + req.Branch
	if _, perr := gitInConfigEnv(ctx, req.RepoDir, credConfig, nil, "push", "--", req.Remote, refspec); perr != nil {
		// A non-fast-forward rejection means the remote moved (spec §30.12, REP-010): re-read the remote
		// to confirm it still holds its diverged commit, and surface a typed divergence — never a force.
		if after, ok, qerr := RemoteRef(ctx, req.RepoDir, req.Remote, req.Branch, credConfig); qerr == nil && ok && after != req.HeadSHA {
			return PushReceipt{}, fmt.Errorf("push %q: %w (remote at %s)", req.Branch, ErrRemoteDiverged, after)
		}
		return PushReceipt{}, fmt.Errorf("push %q: %w", req.Branch, perr)
	}

	// External receipt: re-read the remote ref so the recorded receipt is the remote's own truth, not
	// an assumption (spec §30.9 "provider receipt + remote commit persisted").
	confirmed, ok, err := RemoteRef(ctx, req.RepoDir, req.Remote, req.Branch, credConfig)
	if err != nil {
		return PushReceipt{}, fmt.Errorf("push: confirm remote ref: %w", err)
	}
	if !ok {
		return PushReceipt{}, fmt.Errorf("push %q: remote ref absent after a reported-successful push", req.Branch)
	}
	return PushReceipt{Remote: req.Remote, Branch: req.Branch, RemoteSHA: confirmed}, nil
}

// RemoteRef returns the sha the remote's <branch> ref points at, via `git ls-remote` — the
// reconciliation read (spec §30.9). found is false when the remote has no such branch. It runs under the
// untrusted-repo hardening; credConfig carries the brokered credential (empty for an unauthenticated
// local remote, which never challenges). The remote/branch are guarded against flag-injection.
func RemoteRef(ctx context.Context, repoDir, remote, branch string, credConfig []string) (string, bool, error) {
	if err := rejectGitPositionals(map[string]string{"remote": remote, "branch": branch}); err != nil {
		return "", false, err
	}
	out, err := gitInConfigEnv(ctx, repoDir, credConfig, nil, "ls-remote", remote, "refs/heads/"+branch)
	if err != nil {
		return "", false, err
	}
	fields := strings.Fields(strings.TrimSpace(out))
	if len(fields) < 1 || fields[0] == "" {
		return "", false, nil
	}
	return fields[0], true, nil
}

// --- pull request ---------------------------------------------------------------------------------

// PullRequest is the structured result of an open/find (spec §30.10): the provider PR id, URL, number,
// and whether it is a draft. It is the stable external receipt a duplicate request resolves back to.
type PullRequest struct {
	ID     string
	URL    string
	Number int
	Draft  bool
}

// OpenPRInput is the head/base and model-proposed (policy-filtered) title/body of a pull request.
type OpenPRInput struct {
	HeadBranch string
	Base       string
	Title      string
	Body       string
	Draft      bool
}

// PullRequestClient opens and finds pull requests at the Git provider (spec §30.10). Find returns
// found=false when no OPEN PR exists for (headBranch -> base). The GitHub implementation is the live
// tier; a fake proves the find-before-create idempotency deterministically (REP-008).
type PullRequestClient interface {
	Find(ctx context.Context, headBranch, base string) (PullRequest, bool, error)
	Open(ctx context.Context, in OpenPRInput) (PullRequest, error)
}

// OpenPullRequest opens a DRAFT pull request idempotently (spec §30.10, REP-008): it FINDS an existing
// open PR for (headBranch -> base) BEFORE creating, so a duplicate request or a lost-ack callback
// returns the SAME PR and opens exactly one. The default publication policy opens a draft only — merge
// and non-draft promotion are separate capabilities.
func OpenPullRequest(ctx context.Context, client PullRequestClient, in OpenPRInput) (PullRequest, error) {
	if in.HeadBranch == "" || in.Base == "" {
		return PullRequest{}, fmt.Errorf("open pull request: head branch and base are required")
	}
	if existing, found, err := client.Find(ctx, in.HeadBranch, in.Base); err != nil {
		return PullRequest{}, fmt.Errorf("open pull request: find existing: %w", err)
	} else if found {
		return existing, nil // idempotent: adopt the existing PR rather than open a duplicate (REP-008)
	}
	in.Draft = true // default policy: draft only (§30.8)
	pr, err := client.Open(ctx, in)
	if err != nil {
		return PullRequest{}, fmt.Errorf("open pull request: %w", err)
	}
	return pr, nil
}

// githubPRClient opens/finds pull requests through the GitHub REST API (spec §30.10). It mints its OWN
// short-lived pull_request-scoped installation token per call (reusing the App-broker minting) and
// carries it only in a one-shot Authorization header — never a log or the model context.
//
// ponytail: exercised deterministically against an httptest GitHub double (publish_github_test.go);
// the real github.com round-trip is the gated live wave. Owner/repo come from the binding, not the
// model.
type githubPRClient struct {
	cfg   GitHubAppConfig
	key   *rsa.PrivateKey
	owner string
	repo  string
	now   func() time.Time
}

// NewGitHubPullRequestClient builds a PR client for one repository from the App config. It parses the
// key once (no network) and mints pull_request-scoped tokens lazily per call.
func NewGitHubPullRequestClient(cfg GitHubAppConfig, owner, repo string) (PullRequestClient, error) {
	key, err := parseRSAPrivateKey(cfg.PrivateKeyPEM)
	if err != nil {
		return nil, fmt.Errorf("github pr client: %w", err)
	}
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github pr client: owner and repo are required")
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	return &githubPRClient{cfg: cfg, key: key, owner: owner, repo: repo, now: time.Now}, nil
}

// Find returns the open PR whose head is headBranch and base is base, if any (spec §30.10 idempotency).
func (c *githubPRClient) Find(ctx context.Context, headBranch, base string) (PullRequest, bool, error) {
	token, err := c.token(ctx)
	if err != nil {
		return PullRequest{}, false, err
	}
	// head is qualified owner:branch per the GitHub API; state=open so a merged/closed PR does not block.
	q := url.Values{"state": {"open"}, "head": {c.owner + ":" + headBranch}, "base": {base}}
	endpoint := c.base() + "/pulls?" + q.Encode()
	var prs []githubPR
	if err := c.do(ctx, http.MethodGet, endpoint, token, nil, http.StatusOK, &prs); err != nil {
		return PullRequest{}, false, err
	}
	if len(prs) == 0 {
		return PullRequest{}, false, nil
	}
	return prs[0].toPullRequest(), true, nil
}

// Open creates a draft PR (spec §30.10). The caller (OpenPullRequest) has already found no existing PR.
func (c *githubPRClient) Open(ctx context.Context, in OpenPRInput) (PullRequest, error) {
	token, err := c.token(ctx)
	if err != nil {
		return PullRequest{}, err
	}
	body, _ := json.Marshal(map[string]any{
		"title": in.Title, "body": in.Body, "head": in.HeadBranch, "base": in.Base, "draft": in.Draft,
	})
	var pr githubPR
	if err := c.do(ctx, http.MethodPost, c.base()+"/pulls", token, body, http.StatusCreated, &pr); err != nil {
		return PullRequest{}, err
	}
	return pr.toPullRequest(), nil
}

func (c *githubPRClient) base() string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + "/repos/" + c.owner + "/" + c.repo
}

func (c *githubPRClient) token(ctx context.Context) (string, error) {
	token, _, err := mintGitHubInstallationToken(ctx, c.cfg, c.key, c.now(), ScopePullRequest)
	return token, err
}

// do issues one authenticated GitHub API call and decodes a wantStatus response into out.
func (c *githubPRClient) do(ctx context.Context, method, endpoint, token string, body []byte, wantStatus int, out any) error {
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return fmt.Errorf("build %s %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.cfg.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("%s %s: status %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// githubPR is the subset of the GitHub PR resource the receipt needs.
type githubPR struct {
	ID      int64  `json:"id"`
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	Draft   bool   `json:"draft"`
}

func (g githubPR) toPullRequest() PullRequest {
	return PullRequest{ID: fmt.Sprintf("%d", g.ID), URL: g.HTMLURL, Number: g.Number, Draft: g.Draft}
}
