package stack

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// A repository binding is REMOTE configuration and the console owns it (/repositories). Until 2026-08-05
// `palai up` also created one from PALAI_GIT_CLONE_URL, PALAI_GIT_BASE_BRANCH and PALAI_GIT_REPO in
// .env.local, which made two paths to one binding — the defect this tree spent a week deleting, where
// both paths work and no operator can tell which one their deployment is on.
//
// It was the worse of the two in one specific way: a dotenv binding could not express a connection_ref,
// so it was structurally the binding that could never publish.
//
// This file now refuses the shape that would bring it back, plus the failures the old file existed for
// and which have not changed:
//
//   - the DOTENV PATH returning: a bring-up that POSTs a binding from environment variables.
//   - the SILENT SKIP (E21 T2): no binding, bring-up green, and the agent quietly unable to do the thing
//     the operator installed it for.
//   - "could not ask" being reported as "there are none".

// bindingStub serves GET /v1/repository-bindings with `rows` and records every call, so a test can assert
// what the bring-up did AND what it did not do.
func bindingStub(t *testing.T, rows []map[string]any) (*apiClient, *[]string) {
	t.Helper()
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows})
	}))
	t.Cleanup(srv.Close)
	return &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}, &calls
}

// TestTheBringUpREPORTSBindingsAndCreatesNone is the load-bearing one, and the environment variables are
// set ON PURPOSE: a test that only checked the report would pass against the old code too. What must be
// true is that nothing is WRITTEN while they are set.
func TestTheBringUpREPORTSBindingsAndCreatesNone(t *testing.T) {
	t.Setenv("PALAI_GIT_CLONE_URL", "https://github.com/acme/widgets.git")
	t.Setenv("PALAI_GIT_BASE_BRANCH", "dev")
	t.Setenv("PALAI_GIT_REPO", "acme/widgets")

	api, calls := bindingStub(t, []map[string]any{
		{"id": "repo_abc123", "clone_url": "https://github.com/acme/widgets.git"},
	})
	repo := resolveRepository(api)

	if repo.warn != "" {
		t.Errorf("a deployment WITH a binding still warned: %q", repo.warn)
	}
	if repo.id != "repo_abc123" {
		t.Errorf("id = %q, want the binding the deployment already holds", repo.id)
	}
	if !strings.Contains(repo.resolved, "repo_abc123") {
		t.Errorf("the report does not name the binding: %q", repo.resolved)
	}
	for _, c := range *calls {
		if strings.HasPrefix(c, "POST") {
			t.Errorf("the bring-up WROTE %q with the dotenv variables set: the dotenv path is back", c)
		}
	}
}

// TestNoBindingIsSaidOutLoudAndNamesTheScreen — E21 T2's lesson arriving in this file again: a bring-up
// that quietly does nothing is indistinguishable from one that worked, and the operator otherwise meets
// the gap only as a run answering "no workspace bound for this run".
//
// The message must name the CONSOLE, because that is the only path now. Telling someone to set an
// environment variable nothing reads is exactly how the previous version of this warning became useless.
func TestNoBindingIsSaidOutLoudAndNamesTheScreen(t *testing.T) {
	api, _ := bindingStub(t, nil)
	repo := resolveRepository(api)
	if repo.warn == "" {
		t.Fatal("a deployment with no repository binding was told nothing")
	}
	if !strings.Contains(repo.warn, "/repositories") {
		t.Errorf("the warning does not name the screen that fixes it: %q", repo.warn)
	}
	if strings.Contains(repo.warn, "Set both in .env.local") {
		t.Errorf("the warning still tells the operator to set variables nothing reads: %q", repo.warn)
	}
}

// TestAnUnreadableBindingSurfaceDoesNotClaimThereAreNone — "could not ask" and "there are none" are
// different facts, and only one of them should send an operator to create something they may already
// have. The old file asserted the opposite (it WARNED on a 404), which was right when this function
// created bindings and is wrong now that it only reports them.
func TestAnUnreadableBindingSurfaceDoesNotClaimThereAreNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	api := &apiClient{baseURL: srv.URL, key: "test", http: &http.Client{Timeout: 5 * time.Second}}

	repo := resolveRepository(api)
	if repo.warn != "" {
		t.Errorf("an unreadable surface was reported as an absent binding: %q", repo.warn)
	}
	if repo.id != "" {
		t.Errorf("id = %q from a 404", repo.id)
	}
}

// TestABindingWithNoWayToPublishIsNamed covers the warning that MOVED. It used to live in
// applyGitHubAppEnv gated on PALAI_GIT_CLONE_URL — and when that variable lost its last reader the
// condition became unsatisfiable, so the warning could never fire while still reading as coverage.
//
// The failure it names is silent: an agent proposes a push, a human approves it, and the publication
// waits forever with no error anywhere.
func TestABindingWithNoWayToPublishIsNamed(t *testing.T) {
	api, _ := bindingStub(t, []map[string]any{{"id": "repo_stranded", "connection_ref": ""}})
	notice := api.missingPublisherNotice()
	if notice == "" {
		t.Fatal("a binding with neither a connection_ref nor a GitHub App was not reported")
	}
	if !strings.Contains(notice, "repo_stranded") {
		t.Errorf("the notice does not name the binding that cannot publish: %q", notice)
	}
}

// TestABindingWithItsOwnConnectionIsNotReported — a binding carrying a connection_ref publishes WITHOUT
// the App, so warning about it would be the crying-wolf this file refuses elsewhere.
func TestABindingWithItsOwnConnectionIsNotReported(t *testing.T) {
	api, _ := bindingStub(t, []map[string]any{{"id": "repo_ok", "connection_ref": "gh-token"}})
	if notice := api.missingPublisherNotice(); notice != "" {
		t.Errorf("a binding that can publish on its own was reported as stranded: %q", notice)
	}
}
