package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSlackCapabilityAdvertisedOnlyWhenMounted is the Slack half of the §2 discovery invariant, alongside
// A2A (E17 T2) and capability-workers (E19 T8a): a binary that can serve no Slack workspace must not
// advertise `slack`.
//
// Before E19 T1 the tree had no Slack route at all and capabilities.go named `slack` unconditionally — a
// quieter member of the same class as D14's "capability-workers": "stable". Wiring a route does not fix that
// by itself; deriving the claim from a mount does.
//
// THE MOUNT IT DERIVES FROM CHANGED ON 2026-08-05 AND THE INVARIANT DID NOT. This test used to mount
// WithSlack — the in-process Events API receiver — and probe that POST /v1/slack/events existed. That
// receiver is gone: Slack is dialled by apps/slack-bot, which registers itself through the BOT REGISTRY and
// then reaches this control plane over `/v1` like any other client. So the registry is what makes a Slack
// workspace serveable, and `cfg.bots` is what the claim now derives from.
//
// The mounted half asserts the RECOMPUTED tier, never a literal: mounting makes a capability advertisABLE,
// it never sets or raises its maturity. `slack` stays preview because §6 leg 1 — a receipt from a REAL Slack
// workspace — is untouched by any amount of local wiring.
func TestSlackCapabilityAdvertisedOnlyWhenMounted(t *testing.T) {
	unmounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil)
	if got := capabilityValue(t, unmounted, "slack"); got != "" {
		t.Fatalf("without a mounted bot registry, slack = %q, want absent — a binary whose POST /v1/bots 404s can register no workspace and must not claim the capability (plan §2)", got)
	}

	mounted := NewRouter(fakeVerifier{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, SSEConfig{}, nil, nil,
		WithBots(&fakeBotRegistry{}))
	want := recomputedTier(t, "slack")
	if got := capabilityValue(t, mounted, "slack"); got != want {
		t.Fatalf("with the bot registry mounted, slack = %q, want the recomputed tier %q — wiring makes the claim advertisABLE, the evidence decides the tier", got, want)
	}
	if want != "preview" {
		t.Fatalf("the recomputed slack tier is %q; neither wiring nor a cutover may advance it past preview (§6 leg 1 is an OPERATOR leg, not a code change)", want)
	}
}

// TestSlackHTTPReceiversAreGone is the other side of the same coin, asserted on the ROUTES rather than on
// discovery: the two legacy Slack receivers must not exist on ANY router, however it is configured.
//
// IT IS STRONGER THAN THE TEST IT REPLACES, deliberately. The old one probed a router built WITHOUT
// WithSlack and checked the route was absent — which proves only that an option nobody passed mounts
// nothing. There is no option to withhold any more, so this drives the FULLY mounted router: if some future
// change re-mounts an unauthenticated Slack receiver behind any seam, this fails.
//
// An advertised-but-absent surface and a present-but-unadvertised one are both §2 violations; this pins the
// second, on the surface that used to be able to commit it.
func TestSlackHTTPReceiversAreGone(t *testing.T) {
	// The header the deleted receivers set to tell Slack "do not redeliver this". It is written out here
	// rather than imported because the constant that held it went with them — and that is the point: nothing
	// in this package should be able to produce this header any more.
	const hdrNoRetry = "X-Slack-No-Retry"

	ts := httptest.NewServer(fullyMountedRouter())
	defer ts.Close()

	for _, route := range []string{"/v1/slack/events", "/v1/slack/interactions"} {
		resp, err := http.Post(ts.URL+route, "application/json", strings.NewReader(`{"type":"event_callback","team_id":"T1","event_id":"E1"}`))
		if err != nil {
			t.Fatalf("POST %s: %v", route, err)
		}
		// It falls through to the AUTHENTICATED router, which refuses an unauthenticated caller. The point is
		// that no Slack handler ran: a mounted receiver answered 200, or 400/404 carrying the suppress header
		// that only it ever set.
		if resp.StatusCode == http.StatusOK || resp.Header.Get(hdrNoRetry) != "" {
			resp.Body.Close()
			t.Fatalf("%s answered %d (%s=%q) on a FULLY mounted router — this deployment serves no Slack HTTP transport; apps/slack-bot holds an outbound Socket Mode connection instead",
				route, resp.StatusCode, hdrNoRetry, resp.Header.Get(hdrNoRetry))
		}
		resp.Body.Close()
	}
}
