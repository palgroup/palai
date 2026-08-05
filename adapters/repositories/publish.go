package repositories

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
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
	// OpMergePullRequest is the third side effect the owner asked for, and it is a CHECK value before it is
	// code: publications.operation was `CHECK (operation IN ('push_branch','open_pull_request'))` until
	// 000044 R2 widened it. Everything else on this path — the pending row, the one-shot hash, the human's
	// button, the boundary pump, the receipt — already existed and is reused unchanged.
	OpMergePullRequest PublishOperation = "merge_pull_request"
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
//
// A MERGE takes the push's rule (the default arm) rather than the pull request's, and the difference is
// the whole of E23 T6: a PR is a conversation that follows a branch, but a merge is an act performed on
// ONE commit. Excluding the head would let a re-proposed merge dedupe onto an approval a human gave for
// different content — so the head is part of the identity, and a branch that moved needs a new approval
// to be merged.
func IdempotencyKey(project, runID string, op PublishOperation, remote, branch, base, headSHA string) string {
	var parts []string
	switch op {
	case OpOpenPullRequest:
		parts = []string{project, runID, string(op), remote, branch, base}
	default: // push_branch: the exact head is part of the operation identity
		parts = []string{project, runID, string(op), remote, branch, base, headSHA}
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
func RequestHash(project, runID string, op PublishOperation, remote, branch, base, headSHA string) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{project, runID, string(op), remote, branch, base, headSHA}, "\x00")))
	return "req_" + hex.EncodeToString(sum[:16])
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
		// The push was rejected: re-read the remote to distinguish the cases. A concurrent identical push
		// (or a lost-ack retry) may have landed the SAME head between our reconcile read and our push — the
		// remote is now at HeadSHA, so this is reconciled, not a failure. A DIFFERENT remote head means it
		// moved (spec §30.12, REP-010): surface a typed divergence — never a force. Anything else is the
		// raw push error.
		if after, ok, qerr := RemoteRef(ctx, req.RepoDir, req.Remote, req.Branch, credConfig); qerr == nil && ok {
			if after == req.HeadSHA {
				return PushReceipt{Remote: req.Remote, Branch: req.Branch, RemoteSHA: after, Reconciled: true}, nil
			}
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

// MergeInput is the infrastructure-owned request to merge one pull request (E23 T6). Every field is
// resolved server-side: Number from the run's OWN published open_pull_request receipt, HeadSHA from that
// same publication, Method from the binding's policy. None of the three is reachable by the model, which
// is why the merge tool's input schema is empty.
type MergeInput struct {
	Number  int
	HeadSHA string // sent as `sha`; GitHub answers 409 if the PR head has moved off it
	Method  string // merge | squash | rebase (binding policy; empty defaults to merge)
}

// MergeReceipt is the external receipt of a merge (GitHub's own answer): whether it merged, the merge
// commit sha, and the provider's message.
type MergeReceipt struct {
	Merged  bool
	SHA     string
	Message string
}

// ErrMergeRefused is GitHub's documented 405 — "Method Not Allowed if merge cannot be performed"
// (https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request, fetched 2026-07-29). It is every
// reason a merge can be refused that E23 deliberately does NOT reimplement: a required check that has not
// passed, a required review nobody left, a conflict, a closed PR, a branch protection rule. Palai does not
// query any of them — it asks, and an honest warning is what comes back.
var ErrMergeRefused = errors.New("merge_refused_by_provider")

// ErrHeadMoved is GitHub's documented 409 — "Conflict if sha was provided and pull request head did not
// match" — and it is the reason `sha` is MANDATORY here although the API marks it optional. The head a
// human approved is the only head that may be merged; a branch that moved in the window between the
// button and the boundary earns a refusal rather than a merge of content nobody read.
var ErrHeadMoved = errors.New("pull_request_head_moved")

// mergeMethods is the closed set the API documents for merge_method. An unknown method is refused before
// any call: it comes from operator config, and a typo that silently became "merge" would change how code
// lands in a repository without anybody being told.
var mergeMethods = []string{"merge", "squash", "rebase"}

// PullRequestClient opens, finds and merges pull requests at the Git provider (spec §30.10, E23 T6). Find
// returns found=false when no OPEN PR exists for (headBranch -> base). The GitHub implementation is the
// live tier; a fake proves the find-before-create idempotency deterministically (REP-008).
type PullRequestClient interface {
	Find(ctx context.Context, headBranch, base string) (PullRequest, bool, error)
	Open(ctx context.Context, in OpenPRInput) (PullRequest, error)
	Merge(ctx context.Context, in MergeInput) (MergeReceipt, error)
}

// MergePullRequest merges an approved pull request at exactly the approved head (E23 T6). It validates
// the operator's merge method and requires both a PR number and a head, then hands the call to the
// provider — where the `sha` guard lives. There is no find-before-merge and no CI wait: what may be
// merged is GitHub's decision, and a refusal comes back as ErrMergeRefused for the pump to journal.
func MergePullRequest(ctx context.Context, client PullRequestClient, in MergeInput) (MergeReceipt, error) {
	if in.Number <= 0 || in.HeadSHA == "" {
		return MergeReceipt{}, fmt.Errorf("merge pull request: a pull request number and the approved head are required")
	}
	if in.Method == "" {
		in.Method = "merge"
	}
	if !slices.Contains(mergeMethods, in.Method) {
		return MergeReceipt{}, fmt.Errorf("merge pull request: merge_method %q is not one of %v", in.Method, mergeMethods)
	}
	receipt, err := client.Merge(ctx, in)
	if err != nil {
		return MergeReceipt{}, fmt.Errorf("merge pull request #%d: %w", in.Number, err)
	}
	return receipt, nil
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
// the real github.com round-trip is the gated live wave.
//
// WHERE OWNER/REPO COME FROM, stated exactly, because this comment used to say "Owner/repo come from the
// binding, not the model" and that was FALSE for every deployment — main.go built this client from
// PALAI_GITHUB_REPO / PALAI_GIT_REPO, so one stack could open pull requests against exactly ONE repository
// however many bindings it served. A comment asserting the property the code lacked, on the line a reader
// would check it.
//
// Since 2026-08-01 it is true for a binding that carries its own connection_ref: that path builds this
// client per publication from the binding's repository_identity (RepositoryPublisher.PRClientFor). It is
// still the ENV VAR for a binding without one, which is the deployment-global path and unchanged. So the
// honest sentence is: owner/repo come from the binding when the binding brought its own credential, and
// from the deployment otherwise — never from the model, which is the half that was always true.
// githubEndpoint is where this client talks and with what. It replaced GitHubAppConfig, which carried an
// App id, an installation id, a PEM and a repository allow-list that this file never read — the fields
// existed because one type served both the App broker and the PR client, and the broker is gone.
type githubEndpoint struct {
	BaseURL    string
	HTTPClient *http.Client
}

type githubPRClient struct {
	cfg   githubEndpoint
	owner string
	repo  string
	now   func() time.Time
	// static is the credential this client publishes under: the token a repository binding's
	// connection_ref resolved to. It is the ONLY source since the deployment-global GitHub App was removed
	// (2026-08-05), which is why it has no zero-value branch any more — a client with no token is refused
	// at construction rather than falling back to something deployment-wide.
	static string
}

// NewTokenPullRequestClient builds a PR client over an ALREADY-RESOLVED token, for one owner/repo. It is
// the seam that lets a repository binding's own credential open and merge pull requests, instead of every
// publication in the deployment going out under the global App.
//
// NO KEY IS PARSED because none is needed, and no network call is made here.
func NewTokenPullRequestClient(token, baseURL, owner, repo string) (PullRequestClient, error) {
	if strings.TrimSpace(token) == "" {
		return nil, fmt.Errorf("github pr client: no credential supplied")
	}
	if owner == "" || repo == "" {
		return nil, fmt.Errorf("github pr client: owner and repo are required")
	}
	cfg := githubEndpoint{BaseURL: baseURL}
	if cfg.BaseURL == "" {
		cfg.BaseURL = "https://api.github.com"
	}
	cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	return &githubPRClient{cfg: cfg, owner: owner, repo: repo, now: time.Now, static: token}, nil
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

// Merge performs PUT /repos/{owner}/{repo}/pulls/{n}/merge with `sha` ALWAYS set (E23 T6, P16). The API
// marks `sha` optional; sending it is what turns "the head moved while a human was reading" from a silent
// merge of unapproved content into a 409 that merges nothing. The two documented refusals are mapped to
// typed errors so the pump journals a warning a human can act on rather than an HTTP status.
func (c *githubPRClient) Merge(ctx context.Context, in MergeInput) (MergeReceipt, error) {
	token, err := c.token(ctx)
	if err != nil {
		return MergeReceipt{}, err
	}
	body, _ := json.Marshal(map[string]any{"sha": in.HeadSHA, "merge_method": in.Method})
	var out struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	endpoint := fmt.Sprintf("%s/pulls/%d/merge", c.base(), in.Number)
	if err := c.do(ctx, http.MethodPut, endpoint, token, body, http.StatusOK, &out); err != nil {
		var status *apiStatusError
		if errors.As(err, &status) {
			switch status.status {
			case http.StatusMethodNotAllowed:
				return MergeReceipt{}, fmt.Errorf("%w: %s", ErrMergeRefused, status.body)
			case http.StatusConflict:
				return MergeReceipt{}, fmt.Errorf("%w: head is no longer %s: %s", ErrHeadMoved, in.HeadSHA, status.body)
			}
		}
		return MergeReceipt{}, err
	}
	return MergeReceipt{Merged: out.Merged, SHA: out.SHA, Message: out.Message}, nil
}

func (c *githubPRClient) base() string {
	return strings.TrimRight(c.cfg.BaseURL, "/") + "/repos/" + c.owner + "/" + c.repo
}

// token returns the credential for one API call.
//
// TWO SOURCES, AND THEY DIFFER IN NARROWING RATHER THAN IN FORM. A tenant-supplied token (the binding's
// connection_ref) is returned as-is: its rights are whatever the tenant granted it, and this client cannot
// narrow them — broker.go's tokenBroker says the same of the Git half. The App path mints a token whose
// permissions match ScopePullRequest, so the deployment-global identity stays least-privilege.
//
// A tenant that holds its own credential is choosing to scope it itself. That is the trade the owner asked
// for — a credential provisioned ONCE from the admin panel instead of an App installation per machine —
// and stating it here is better than quietly implying a narrowing that does not happen.
// token returns the binding's own credential. It takes a context it no longer uses, and keeping the
// signature is deliberate: every caller is on a request path, and a token source that cannot be
// cancelled today may be one that dials tomorrow.
func (c *githubPRClient) token(context.Context) (string, error) { return c.static, nil }

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
		return &apiStatusError{call: method + " " + endpoint, status: resp.StatusCode, body: strings.TrimSpace(string(msg))}
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// apiStatusError carries the status code a GitHub call answered with, so a caller can tell the DOCUMENTED
// refusals apart from an outage: merge's 405 ("merge cannot be performed") and 409 ("head did not match")
// mean different things to a human, and a string-matched status is the guard that quietly stops matching.
// The body is the provider's own message; a brokered installation token never appears in it.
type apiStatusError struct {
	call   string
	status int
	body   string
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("%s: status %d: %s", e.call, e.status, e.body)
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
