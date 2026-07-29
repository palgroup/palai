package stack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// E23 T2 — `palai up` says out loud what is open.
//
// This is E21 T2's silent-SKIP lesson applied to a security posture rather than a capability: the project
// approver list is UNRESTRICTED when absent, deliberately, so a bring-up that says nothing leaves an
// operator believing the approve surfaces are gated when the only gate is possession of a tenant-scoped
// API key.
//
// It is the mirror image of slackApproverWarning next to it. That one warns because an unset list refuses
// EVERYTHING in silence; this one warns because an unset list refuses NOTHING.

// approverStack serves the one read approverListWarning makes, with a canned config_policy.
func approverStack(t *testing.T, status int, configPolicy string) *apiClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/projects/"+bootstrapProjectID {
			t.Errorf("palai up read %s, want /v1/projects/%s — the warning must be about the project this key is scoped to",
				r.URL.Path, bootstrapProjectID)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"id":"` + bootstrapProjectID + `","config_policy":` + configPolicy + `}`))
	}))
	t.Cleanup(ts.Close)
	return &apiClient{baseURL: ts.URL, key: "not-a-real-key", http: &http.Client{Timeout: 5 * time.Second}}
}

// TestApproverListAbsenceIsSaidOutLoud is the requirement, verbatim: a stack with no approver list must
// name what is open, and it must name the HTTP surface specifically — that is the one gated by nothing but
// key possession, while Slack clicks still meet the connection's own allowed_users.
func TestApproverListAbsenceIsSaidOutLoud(t *testing.T) {
	// The state of every project alive today: config_policy is NULL and renders as JSON null.
	for _, policy := range []string{`null`, `{}`, `{"allowed_models":["m"]}`, `{"approvers":[]}`} {
		warn := approverListWarning(approverStack(t, http.StatusOK, policy))
		if warn == "" {
			t.Fatalf("config_policy %s produced no warning; an operator learns of the open surface only by being surprised", policy)
		}
		for _, want := range []string{
			"no project approver list is configured",
			"the HTTP approve surface is gated only by tenant-scoped key possession",
			"palai admin project set-policy", // the fix, not just the diagnosis
		} {
			if !strings.Contains(warn, want) {
				t.Fatalf("the warning for %s must contain %q, got: %q", policy, want, warn)
			}
		}
	}
}

// TestApproverListPresenceIsSilent: a configured stack must not be nagged. A warning that fires when the
// thing it warns about has been done is a warning operators learn to ignore.
func TestApproverListPresenceIsSilent(t *testing.T) {
	warn := approverListWarning(approverStack(t, http.StatusOK, `{"approvers":["key:key_9f2c"]}`))
	if warn != "" {
		t.Fatalf("a project WITH an approver list warned anyway: %q", warn)
	}
}

// TestApproverListUnreadableWarnsAboutTheREADNotTheLIST is the honesty half, and it is the one a
// copy-pasted warning would get wrong. "I could not tell" and "there is no list" are different facts, and
// reporting the second when the first is true would be exactly the over-claiming this warning exists to
// end — in the opposite direction.
func TestApproverListUnreadableWarnsAboutTheREADNotTheLIST(t *testing.T) {
	warn := approverListWarning(approverStack(t, http.StatusInternalServerError, `null`))
	if warn == "" {
		t.Fatal("a failed read produced no warning at all; silence here means an operator is told nothing about a surface nobody checked")
	}
	if strings.Contains(warn, "no project approver list is configured") {
		t.Fatalf("a FAILED READ was reported as an ABSENT LIST: %q", warn)
	}
	if !strings.Contains(warn, "could not read") {
		t.Fatalf("the warning must say the read failed, got: %q", warn)
	}
}
