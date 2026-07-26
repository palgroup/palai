//go:build live

// The A2A push-delivery live smoke (E19 T9). Like its Slack siblings it is written NOW and runs UNCHANGED
// the moment the owner supplies a receiver — that is the phase's shape: correctness comes from the
// published specification, and the live leg exists to settle what a document cannot.
//
// It settles exactly one thing, and it is the reason this file exists:
//
//	D11 (plan §3.5) — the A2A specification defines PushNotificationConfig.token as an optional field
//	"for client-side validation" but NAMES NO HEADER to carry it. Our header (a2a.PushTokenHeader) is
//	therefore OUR CHOICE, and nothing local can tell us whether a foreign receiver looks there. Only a
//	real third-party receiver can, and TestLiveA2APushReachesFilteredThroughTheRealPolicy asks it
//	directly by delivering a real StreamResponse and reporting what it sent.
//
// WHAT IT STILL DOES NOT MAKE TRUE: a receiver that accepts our POST is not interop. Interop is a foreign
// PEER implementing the A2A server side, which is §6 leg 2 and which this leg does not close. The honest
// output of a green run here is "our delivery reached a third-party HTTPS receiver with the shape and the
// header we chose", nothing more.
//
// Without A2A_PUSH_WEBHOOK_URL it SKIPS, naming the variable and the §0 handover row that supplies it.
package live

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/a2a"
	"github.com/palgroup/palai/adapters/integrations/webhook"
)

// need returns an env var's value or skips, naming where §0 says to get it — the same contract every Slack
// live leg uses, so a partial handover reports partial-green rather than a red wall.
func need(t *testing.T, name, where string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set — supply it from E19 plan §0 (%s) and re-run; no code changes are needed", name, where)
	}
	return v
}

// TestLiveA2APushReachesFilteredThroughTheRealPolicy delivers ONE real StreamResponse to the owner-supplied
// receiver through the PRODUCTION WebhookPusher — the same egress-vetted, IP-pinned sender the §21.6
// delivery pump uses, under the same host allowlist a deployment would configure.
//
// The allowlist is derived from the supplied URL rather than left empty on purpose: an empty allowlist is
// the weaker posture the specification's security guidance warns about, and a live leg that quietly ran in
// the weaker posture would be measuring something no deployment should run.
func TestLiveA2APushReachesFilteredThroughTheRealPolicy(t *testing.T) {
	raw := need(t, "A2A_PUSH_WEBHOOK_URL",
		"§0.2 — OPTIONAL: an https receiver the owner controls; the deterministic loopback proof is complete without it")

	target, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || target.Hostname() == "" {
		t.Fatalf("A2A_PUSH_WEBHOOK_URL is not a usable URL (the value is never logged): parse error=%v", err)
	}
	if target.Scheme != "https" {
		t.Fatalf("A2A_PUSH_WEBHOOK_URL must be https — the egress policy refuses a public http destination, and a live leg must not be run in a posture no deployment should use")
	}

	dead := make(chan a2a.PushFailure, 1)
	pusher := a2a.NewWebhookPusher(webhook.NewSender(), a2a.PushPolicy{
		// The host allowlist is the D12 mitigation, exercised rather than skipped.
		AllowedHosts: []string{target.Hostname()},
		MaxAttempts:  3,
		TimeoutMS:    10000,
	})
	pusher.DeadLetter = func(_ context.Context, f a2a.PushFailure) { dead <- f }

	// CONTRACT https://a2a-protocol.org/latest/specification/ (checked 2026-07-26): the server POSTs a
	// StreamResponse — task | message | statusUpdate | artifactUpdate — never a bare Task.
	now := time.Now().UTC().Format(time.RFC3339)
	resp := a2a.StreamResponse{Task: &a2a.Task{
		ID: "live-push-" + now, Kind: "task",
		Status: a2a.TaskStatus{State: a2a.TaskStateCompleted, Timestamp: now},
	}}

	cfg := a2a.PushNotificationConfig{URL: target.String(), Token: os.Getenv("A2A_PUSH_WEBHOOK_TOKEN")}
	if err := pusher.Push(context.Background(), cfg, resp); err != nil {
		t.Fatalf("the push was refused before delivery (policy/egress): %v", err)
	}
	pusher.Wait()

	stats := pusher.Stats()
	select {
	case f := <-dead:
		// The failure record carries no URL and no token by construction; printing it is safe.
		t.Fatalf("the notification was dead-lettered after %d attempt(s): status=%d err=%v — the receiver did not accept our POST",
			f.Attempts, f.StatusCode, f.Err)
	default:
	}
	if stats.Delivered != 1 {
		t.Fatalf("delivered=%d refused=%d dead=%d, want exactly one delivery", stats.Delivered, stats.Refused, stats.Dead)
	}

	body, _ := json.Marshal(resp)
	t.Logf("DELIVERED to the owner-supplied receiver: %d byte StreamResponse, token carried in %q (D11: the specification names NO header for it, so this is OUR choice and a receiver that looks elsewhere will not see it)",
		len(body), a2a.PushTokenHeader)
	t.Log("HONEST CEILING: a receiver accepting our POST is NOT interop — a foreign A2A PEER implementing the server side is §6 leg 2 and this leg does not close it")
}
