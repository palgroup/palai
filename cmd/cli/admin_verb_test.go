package main

// `palai admin runner …` (E24 T5). The admin package's own tests prove the resource and the routes; this
// one proves the VERB a runbook tells an operator to type reaches them, because dispatch() is the only
// place that decision lives and nothing else in this package's tests exercises it.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAdminVerbReachesTheRunnerLifecycle drives the top-level dispatcher for each of the four verbs. The
// stub records the method and path, so a mis-spelled prefix (or a `palai runner` that would read as an
// operation on the local runner CONTAINER) shows up as a wrong path rather than as a passing test.
func TestAdminVerbReachesTheRunnerLifecycle(t *testing.T) {
	var got struct{ method, path string }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.method, got.path = r.Method, r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"rnr_one","object":"runner","state":"cordoned"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("PALAI_BASE_URL", srv.URL)
	t.Setenv("PALAI_API_KEY", "admin-key")

	for _, tc := range []struct {
		args                 []string
		wantMethod, wantPath string
	}{
		{[]string{"admin", "runner", "list"}, "GET", "/v1/runners"},
		{[]string{"admin", "runner", "cordon", "rnr_one"}, "POST", "/v1/runners/rnr_one/cordon"},
		{[]string{"admin", "runner", "resume", "rnr_one"}, "POST", "/v1/runners/rnr_one/resume"},
		{[]string{"admin", "runner", "revoke", "rnr_one"}, "POST", "/v1/runners/rnr_one/revoke"},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			got.method, got.path = "", ""
			if err := dispatch(tc.args); err != nil {
				t.Fatalf("palai %s: %v", strings.Join(tc.args, " "), err)
			}
			if got.method != tc.wantMethod || got.path != tc.wantPath {
				t.Fatalf("reached %s %s, want %s %s", got.method, got.path, tc.wantMethod, tc.wantPath)
			}
		})
	}
}

// TestAdminVerbWithNoResourceIsAnError keeps the dispatcher honest about the argument it needs: `palai
// admin` alone must say so rather than panic on args[1].
func TestAdminVerbWithNoResourceIsAnError(t *testing.T) {
	if err := dispatch([]string{"admin"}); err == nil {
		t.Fatal("`palai admin` with no resource was accepted")
	}
}
