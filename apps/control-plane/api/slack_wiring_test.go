package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSlackCapabilityAdvertisedOnlyWhenMounted is the Slack half of the §2 discovery invariant, the third
// instance of the pattern after A2A (E17 T2) and capability-workers (E19 T8a): a binary that serves no
// Slack receiver must not advertise `slack`.
//
// Before E19 T1 the tree had no Slack route at all and capabilities.go named `slack` unconditionally — a
// quieter member of the same class as D14's "capability-workers": "stable". Wiring the route does not fix
// that by itself; deriving the claim from the mount does.
//
// The mounted half asserts the RECOMPUTED tier, never a literal: mounting makes a capability advertisABLE,
// it never sets or raises its maturity. `slack` stays preview because §6 leg 1 — a receipt from a REAL Slack
// workspace — is untouched by any amount of local wiring.
func TestSlackCapabilityAdvertisedOnlyWhenMounted(t *testing.T) {
	unmounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
	if got := capabilityValue(t, unmounted, "slack"); got != "" {
		t.Fatalf("without a mounted Slack receiver, slack = %q, want absent — a binary whose POST /v1/slack/events 404s must not claim the capability (plan §2)", got)
	}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithSlack(newSlackBridge([]byte("wiring-probe"))))
	want := recomputedTier(t, "slack")
	if got := capabilityValue(t, mounted, "slack"); got != want {
		t.Fatalf("with the Slack receiver mounted, slack = %q, want the recomputed tier %q — wiring makes the claim advertisABLE, the evidence decides the tier", got, want)
	}
	if want != "preview" {
		t.Fatalf("the recomputed slack tier is %q; E19 wiring must not advance it past preview (§6 leg 1 is an OPERATOR leg, not a code change)", want)
	}
}

// TestSlackRouteUnmountedWhenNoBridge is the other side of the same coin, asserted on the ROUTE rather than
// on discovery: without WithSlack the endpoint must not exist. An advertised-but-absent surface and a
// present-but-unadvertised one are both §2 violations; this pins the second.
func TestSlackRouteUnmountedWhenNoBridge(t *testing.T) {
	ts := httptest.NewServer(NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil))
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/slack/events", "application/json", strings.NewReader(`{"type":"event_callback","team_id":"T1","event_id":"E1"}`))
	if err != nil {
		t.Fatalf("POST /v1/slack/events: %v", err)
	}
	defer resp.Body.Close()
	// It falls through to the AUTHENTICATED router, which refuses an unauthenticated caller — the point is
	// only that no Slack handler ran (a mounted one answers 400/404 with the suppress header).
	if resp.StatusCode == http.StatusOK || resp.Header.Get(hdrNoRetry) != "" {
		t.Fatalf("unmounted Slack route answered %d (%s=%q); the handler must not exist without WithSlack",
			resp.StatusCode, hdrNoRetry, resp.Header.Get(hdrNoRetry))
	}
}
