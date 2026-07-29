package repositories

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestGitHubPRClientFindBeforeCreate proves the GitHub pull-request client's transport against a fake
// GitHub API (spec §30.10), deterministically — the real github.com round-trip is the gated live wave.
// It mints a pull_request-scoped App token, and OpenPullRequest FINDS before it creates: the first call
// opens one draft PR, the second finds it and opens none (REP-008). The installation token rides only
// the Authorization header, never a leaked field.
func TestGitHubPRClientFindBeforeCreate(t *testing.T) {
	var opens int
	var existing *githubPR
	var sawDraft bool
	var authTokens = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "); bearer != "" {
			authTokens[bearer] = true
		}
		switch {
		// The App-JWT -> installation-token exchange (reused from the broker minting path).
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fakeInstallationToken, "expires_at": "2999-01-01T00:00:00Z"})
		// Find: return the existing PR if one was opened, else an empty list.
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls"):
			out := []githubPR{}
			if existing != nil {
				out = append(out, *existing)
			}
			_ = json.NewEncoder(w).Encode(out)
		// Open: create exactly one PR and remember it so a later Find returns it.
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/pulls"):
			var body struct {
				Draft bool `json:"draft"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			sawDraft = body.Draft
			opens++
			pr := githubPR{ID: 4242, Number: 7, HTMLURL: "https://example.test/o/r/pull/7", Draft: body.Draft}
			existing = &pr
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(pr)
		default:
			http.Error(w, "unexpected", http.StatusNotFound)
		}
	}))
	defer srv.Close()

	client, err := NewGitHubPullRequestClient(GitHubAppConfig{
		AppID: "12345", InstallationID: "67890", PrivateKeyPEM: testRSAKeyPEM(t),
		Repositories: []string{"repo"}, BaseURL: srv.URL, HTTPClient: srv.Client(),
	}, "o", "r")
	if err != nil {
		t.Fatalf("NewGitHubPullRequestClient() error = %v", err)
	}
	in := OpenPRInput{HeadBranch: "agent/s/r", Base: "main", Title: "t", Body: "b"}

	first, err := OpenPullRequest(context.Background(), client, in)
	if err != nil {
		t.Fatalf("first OpenPullRequest error = %v", err)
	}
	if !sawDraft || !first.Draft {
		t.Fatal("the opened PR must be a draft (default publication policy, §30.8)")
	}
	second, err := OpenPullRequest(context.Background(), client, in)
	if err != nil {
		t.Fatalf("second OpenPullRequest error = %v", err)
	}
	if second.ID != first.ID || second.Number != 7 {
		t.Fatalf("duplicate request returned a different PR (%+v vs %+v); want the same (REP-008)", second, first)
	}
	if opens != 1 {
		t.Fatalf("PR opens = %d, want exactly 1 (find-before-create against the real transport)", opens)
	}
	if authTokens[fakeInstallationToken] != true {
		t.Fatal("the client must authenticate with the minted installation token")
	}
}

// mergeDouble is a GitHub API double built to the published *Merge a pull request* contract
// (https://docs.github.com/en/rest/pulls/pulls#merge-a-pull-request, fetched 2026-07-29). It answers the
// two documented refusals for the two documented reasons: **409** when a `sha` was provided and the pull
// request head does not match it, **405** when the merge cannot be performed. head is what the branch is
// currently at — moving it between the approval and the call is the race this whole design exists for.
type mergeDouble struct {
	head     string
	refuse   bool     // answer 405 the way branch protection, a failing check or a conflict does
	requests []string // the raw bodies the API received, so a test can assert what was SENT
	path     string
}

func newMergeServer(t *testing.T, d *mergeDouble) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens") {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"token": fakeInstallationToken, "expires_at": "2999-01-01T00:00:00Z"})
			return
		}
		if r.Method != http.MethodPut || !strings.HasSuffix(r.URL.Path, "/merge") {
			http.Error(w, "unexpected", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		d.requests, d.path = append(d.requests, string(body)), r.URL.Path
		var in struct {
			SHA    string `json:"sha"`
			Method string `json:"merge_method"`
		}
		_ = json.Unmarshal(body, &in)
		switch {
		case d.refuse:
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Required status check \"ci\" is expected."})
		case in.SHA != "" && in.SHA != d.head:
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Head branch was modified. Review and try the merge again."})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"sha": "merged_" + in.SHA, "merged": true, "message": "Pull Request successfully merged"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newMergeClient(t *testing.T, srv *httptest.Server) PullRequestClient {
	t.Helper()
	client, err := NewGitHubPullRequestClient(GitHubAppConfig{
		AppID: "12345", InstallationID: "67890", PrivateKeyPEM: testRSAKeyPEM(t),
		Repositories: []string{"repo"}, BaseURL: srv.URL, HTTPClient: srv.Client(),
	}, "o", "r")
	if err != nil {
		t.Fatalf("NewGitHubPullRequestClient() error = %v", err)
	}
	return client
}

// TestGitHubMergeAlwaysSendsTheApprovedSHA is E23 T6's RED #2 at the vendor boundary, and it is the whole
// argument for making an OPTIONAL field MANDATORY. GitHub documents `sha` as "SHA that pull request head
// must match to allow merge" and does not require it; a merge sent without it merges whatever the branch
// happens to point at when the request lands. That is exactly the window a human approval opens — somebody
// reads a message, thinks, and presses a button, and a push can arrive in between.
//
// So the client ALWAYS sends it, and the answer to a moved head is a 409 that merges nothing. The vendor
// hands the race guard over for free; not using it would have been a choice.
func TestGitHubMergeAlwaysSendsTheApprovedSHA(t *testing.T) {
	const approvedHead = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	d := &mergeDouble{head: approvedHead}
	client := newMergeClient(t, newMergeServer(t, d))

	receipt, err := MergePullRequest(context.Background(), client, MergeInput{Number: 12, HeadSHA: approvedHead, Method: "squash"})
	if err != nil {
		t.Fatalf("MergePullRequest() error = %v", err)
	}
	if !receipt.Merged || receipt.SHA != "merged_"+approvedHead {
		t.Fatalf("merge receipt = %+v, want the provider's own merged sha", receipt)
	}
	if len(d.requests) != 1 {
		t.Fatalf("the provider received %d merge request(s), want 1", len(d.requests))
	}
	var sent map[string]any
	if err := json.Unmarshal([]byte(d.requests[0]), &sent); err != nil {
		t.Fatalf("the merge body is not JSON: %v", err)
	}
	if sent["sha"] != approvedHead {
		t.Fatalf("the merge body carried sha=%v, want the approved head %q — an omitted sha merges whatever "+
			"the branch points at when the request lands, which is the one thing an approval cannot survive",
			sent["sha"], approvedHead)
	}
	if sent["merge_method"] != "squash" {
		t.Fatalf("the merge body carried merge_method=%v, want the binding policy's %q", sent["merge_method"], "squash")
	}
	if !strings.HasSuffix(d.path, "/pulls/12/merge") {
		t.Fatalf("the merge went to %q, want /pulls/12/merge", d.path)
	}

	// THE RACE. The branch advances after the approval; the SAME approved sha is sent, and GitHub refuses.
	d.head = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := MergePullRequest(context.Background(), client, MergeInput{Number: 12, HeadSHA: approvedHead, Method: "merge"}); !errors.Is(err, ErrHeadMoved) {
		t.Fatalf("a merge of a MOVED head returned %v, want ErrHeadMoved — the approved commit is the only "+
			"commit that may be merged", err)
	}
}

// TestGitHubMergeRefusalIsHonest is the ceiling stated as a test: everything that can stop a merge —
// required checks, required reviews, branch protection, a conflict — is GitHub's business and comes back as
// the documented 405. Palai queries none of it and does not pretend to; what it owes is an honest, typed
// refusal the pump can journal as a warning a human can act on.
func TestGitHubMergeRefusalIsHonest(t *testing.T) {
	d := &mergeDouble{head: "cccc", refuse: true}
	client := newMergeClient(t, newMergeServer(t, d))
	_, err := MergePullRequest(context.Background(), client, MergeInput{Number: 3, HeadSHA: "cccc"})
	if !errors.Is(err, ErrMergeRefused) {
		t.Fatalf("a 405 became %v, want ErrMergeRefused", err)
	}
	if !strings.Contains(err.Error(), "Required status check") {
		t.Fatalf("the refusal dropped the provider's own reason: %v — an operator reading a warning that says "+
			"only \"405\" has to go and look", err)
	}
}

// TestMergeRefusesAnUnknownMethodBeforeCalling proves the closed set is closed. merge_method comes from
// operator config, so a typo is the likely failure; defaulting it to "merge" would change how work lands in
// a repository silently, and sending it would let the provider decide what a misspelling means.
func TestMergeRefusesAnUnknownMethodBeforeCalling(t *testing.T) {
	client := newFakePRClient()
	if _, err := MergePullRequest(context.Background(), client, MergeInput{Number: 1, HeadSHA: "a", Method: "fast-forward"}); err == nil {
		t.Fatal("an unknown merge_method was accepted")
	}
	if len(client.merges) != 0 {
		t.Fatalf("an unknown merge_method reached the provider: %+v", client.merges)
	}
	// Empty is the documented default and DOES call, with "merge".
	if _, err := MergePullRequest(context.Background(), client, MergeInput{Number: 1, HeadSHA: "a"}); err != nil {
		t.Fatalf("an unset merge_method must default to merge: %v", err)
	}
	if len(client.merges) != 1 || client.merges[0].Method != "merge" {
		t.Fatalf("merges = %+v, want exactly one with method=merge", client.merges)
	}
	// And a merge with no pull request number never reaches the provider either.
	if _, err := MergePullRequest(context.Background(), client, MergeInput{HeadSHA: "a"}); err == nil {
		t.Fatal("a merge with no pull request number was accepted")
	}
	if len(client.merges) != 1 {
		t.Fatalf("a numberless merge reached the provider: %+v", client.merges)
	}
}
