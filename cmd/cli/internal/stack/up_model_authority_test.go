package stack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// panel-credentials — THE TWO AUTHORITIES FOR ONE MODEL CREDENTIAL, said out loud.
//
// There are two, and until this warning existed the precedence between them was SILENT:
//
//	the FILE      OPENAI_API_KEY in .env.local -> the 0600 provider-one file secret ->
//	              PALAI_SECRET_PROVIDER_ONE -> modelBrokerFromEnv's DEPLOYMENT-DEFAULT route.
//	the PLATFORM  a secret ref + a model connection + a PUBLISHED `default` route revision — all three of
//	              which the console now writes.
//
// coordinator.ProjectModelRoute resolves the platform's answer and returns found=false when the project has
// none, at which point the caller falls back to the deployment default. So the platform WINS where it has an
// answer, and a bring-up that re-seals the file's key and prints PROVEN LIVE told the operator nothing about
// which of the two just served their run. That is the defect this file pins shut.
//
// THE TESTS DRIVE THE REAL READS against an httptest control plane rather than a stubbed struct, because the
// warning's whole job is to report what the RUNNING STACK says — a version that read a local variable would
// be reporting this command's own beliefs, which is the failure it exists to correct.

// modelRouteStack serves the three reads publishedModelRoute makes. `resolved` selects which revision
// carries resolved_by_dispatch; an empty string means NONE does, which is a real state — revisions can be
// stored and published and still not be the one dispatch lands on.
func modelRouteStack(t *testing.T, routeName, resolved string) *apiClient {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/v1/model-routes":
			if routeName == "" {
				_, _ = w.Write([]byte(`{"data":[]}`))
				return
			}
			_, _ = w.Write([]byte(`{"data":[{"id":"mroute_1","name":"` + routeName + `"}]}`))
		case r.URL.Path == "/v1/model-routes/mroute_1/revisions":
			// TWO revisions, and only one can be resolved. A fixture with a single row could not catch a
			// reader that took `data[0]` instead of the flagged one — which is precisely the "second opinion
			// about which revision steers" this code must not have.
			_, _ = w.Write([]byte(`{"data":[
				{"model":"gpt-4o-mini","connection_id":"mconn_old","resolved_by_dispatch":false},
				{"model":"claude-sonnet-5","connection_id":"mconn_live","resolved_by_dispatch":` +
				map[bool]string{true: "true", false: "false"}[resolved != ""] + `}
			]}`))
		case strings.HasPrefix(r.URL.Path, "/v1/model-connections/"):
			_, _ = w.Write([]byte(`{"id":"mconn_live","provider":"provider-two","secret_ref":"anthropic-key"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(ts.Close)
	return &apiClient{baseURL: ts.URL, key: "not-a-real-key", http: &http.Client{Timeout: 5 * time.Second}}
}

// TestPublishedRouteIsAnnouncedAsTheOtherAuthority is the requirement: when BOTH exist, the operator must be
// told that the file they just edited is not what their runs use, and told where the live choice actually
// lives.
func TestPublishedRouteIsAnnouncedAsTheOtherAuthority(t *testing.T) {
	warn := modelAuthorityWarning(modelRouteStack(t, defaultModelRouteAlias, "yes"), env(credentialEnv, "sk-test"))
	if warn == "" {
		t.Fatal("a stack with BOTH a file credential and a published route said nothing; the precedence stays silent, which is the whole defect")
	}
	// It must name the variable the operator set, the model actually being served, and the credential's
	// HANDLE — the three facts needed to understand that the file is not in play.
	for _, want := range []string{credentialEnv, "claude-sonnet-5", "anthropic-key", "NOT"} {
		if !strings.Contains(warn, want) {
			t.Fatalf("the warning must name %q so the operator can act on it, got: %s", want, warn)
		}
	}
	// AND IT MUST NAME THE CONSOLE, because "your file is ignored" without "here is where the live choice
	// is made" is a warning that leaves an operator with nowhere to go.
	if !strings.Contains(warn, "/registry") {
		t.Fatalf("the warning must say where the live choice is made, got: %s", warn)
	}
	// THE CREDENTIAL VALUE NEVER APPEARS. The env lookup carries it; this string is printed to a terminal
	// and may be pasted into an issue.
	if strings.Contains(warn, "sk-test") {
		t.Fatalf("the warning carries the credential VALUE: %s", warn)
	}
}

// TestOneAuthorityIsNotWarnedAbout: the warning fires only when there is something to be wrong about. Both
// single-authority stacks are ordinary, supported postures and a line about either is noise that trains an
// operator to skim the ones that matter.
func TestOneAuthorityIsNotWarnedAbout(t *testing.T) {
	// A file credential and NO published route: the file IS the authority. Nothing to announce.
	if warn := modelAuthorityWarning(modelRouteStack(t, "", ""), env(credentialEnv, "sk-test")); warn != "" {
		t.Fatalf("a stack with no model route was warned about: %s", warn)
	}
	// A published route and NO file credential: the route is the only authority. Nothing to announce.
	if warn := modelAuthorityWarning(modelRouteStack(t, defaultModelRouteAlias, "yes"), env()); warn != "" {
		t.Fatalf("a stack with no file credential was warned about: %s", warn)
	}
}

// TestOnlyTheDispatCHResolvedRevisionCounts: an alias whose revisions are all stored-but-not-resolved routes
// NOTHING, so the deployment default still serves and the file is still the authority. Warning there would
// tell an operator their key is unused when it is the only thing being used.
func TestOnlyTheDispatchResolvedRevisionCounts(t *testing.T) {
	if warn := modelAuthorityWarning(modelRouteStack(t, defaultModelRouteAlias, ""), env(credentialEnv, "sk-test")); warn != "" {
		t.Fatalf("a route with no dispatch-resolved revision was reported as overriding the file: %s", warn)
	}
}

// TestANonDefaultAliasIsNotTheAuthority: dispatch resolves exactly ONE alias
// (coordinator.DefaultModelRouteAlias). A route by any other name is stored, publishable, and consulted by
// no run — internal/store/model_routes.go refuses to create one for exactly that reason — so finding one
// must NOT be read as "the platform is serving this project".
//
// This is the arm that would silently pass on a reader that took the first route in the list.
func TestANonDefaultAliasIsNotTheAuthority(t *testing.T) {
	if warn := modelAuthorityWarning(modelRouteStack(t, "experimental", "yes"), env(credentialEnv, "sk-test")); warn != "" {
		t.Fatalf("a route named something other than %q was treated as the live authority: %s", defaultModelRouteAlias, warn)
	}
}

// TestAnUnreadableRouteIsReportedRatherThanAssumedAway: "I could not tell" and "there is no route" are
// different facts and only one of them is about the credential. A warning that silently becomes no-warning
// on a read failure is worse than none — it reads as a clean bill of health.
func TestAnUnreadableRouteIsReportedRatherThanAssumedAway(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(ts.Close)
	api := &apiClient{baseURL: ts.URL, key: "not-a-real-key", http: &http.Client{Timeout: 5 * time.Second}}
	warn := modelAuthorityWarning(api, env(credentialEnv, "sk-test"))
	if warn == "" {
		t.Fatal("an unreadable model route produced no warning; the operator reads that as 'the file is in charge'")
	}
	if !strings.Contains(warn, "could not read") {
		t.Fatalf("the warning must say the READ failed rather than assert anything about the credential, got: %s", warn)
	}
}
