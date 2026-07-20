package repositories

import (
	"context"
	"encoding/json"
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
