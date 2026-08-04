//go:build live

// The Slack interactivity live smoke (E19 T2). Written now, runs UNCHANGED when the owner supplies
// credentials, and SKIPS — naming the missing variable and the §0.1 handover row that supplies it — rather
// than failing, so a partial handover reports partial-green.
//
// What these settle that no fixture can:
//
//	The approval message we construct is one REAL Slack accepts. Block Kit is validated server-side: a
//	malformed block, a bad action id, an over-long value — none of that is visible locally, because our fake
//	peer answers {"ok":true} to anything. Only slack.com can refuse it.
//
//	That chat.update REPAIRS the visible message rather than posting a second one — a fake peer records what
//	it was asked to do, while a real workspace shows what actually happened.
//
// WHAT THE 2026-08-05 CUTOVER TOOK OUT OF THIS FILE, named rather than quietly dropped: a second leg
// (TestLiveSlackButtonClickIsFormEncodedAndVerifies) settled D8 — that a real interactivity POST is
// form-encoded with a single `payload` parameter and that the v0 signature over the RAW form bytes verifies.
// It stood up an HTTP receiver and asked the operator to point the app's Request URL at it. This deployment
// no longer mounts an interactivity receiver: apps/slack-bot takes clicks over Socket Mode, which carries no
// signature and no form encoding, so the leg measured a transport nothing here serves.
//
// D8 IS THEREFORE UNSETTLED AGAIN, and that is the honest state rather than a regression to hide: the v0
// verifier still ships in adapters/integrations/slack and `signing_secret_ref` still sits on the bot row for
// a future HTTP transport (see apps/slack-bot botrow.go). Whoever mounts that transport re-earns the receipt;
// until then nothing in this tree depends on the answer.
package live

import (
	"context"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// slackAPIBase is Slack's Web API root; PALAI_SLACK_API_BASE_URL overrides it for a proxied environment.
func slackAPIBase() string {
	if base := os.Getenv("PALAI_SLACK_API_BASE_URL"); base != "" {
		return base
	}
	return "https://slack.com/api"
}

// TestLiveSlackApprovalMessageIsPostedAndRepaired is the cheapest real-workspace receipt in this task and the
// one the owner can run with two variables: post the minted approval message into a real channel, then repair
// that very message in place with chat.update.
//
// WHAT THIS MEASURES CHANGED IN E23 T3 WITHOUT A LINE OF IT MOVING, and that is worth writing down. From E19
// until then, slack.ApprovalMessage had NO production caller: this smoke proved that a body nothing sent was
// one Slack would accept. slack_approval_post.go now composes exactly this call — the same minter, the same
// two arguments, the same sender and the same pacer — for every approval a run parks on, so the Block Kit
// surface this settles is the surface a human is actually shown.
//
// THE HONEST GAP, since it would be easy to overclaim: what runs here is still the CALL, not the PUMP. The
// claim-and-post loop, the exactly-once row and the destination frozen at enqueue are proven against a real
// PostgreSQL in the component tier; a live leg that drove the pump end to end would need a stack, a
// registered connection and a correlated thread, which is the E23 T7 journey's job and not this file's.
//
// CONTRACT: https://docs.slack.dev/apis/web-api/rate-limits/ (checked 2026-07-26) — chat.postMessage is the
// Special Tier (~1 message per second per channel), which is why the pacer is exercised here too rather than
// only in a unit test.
func TestLiveSlackApprovalMessageIsPostedAndRepaired(t *testing.T) {
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.1 — App → OAuth & Permissions → Bot User OAuth Token; scope chat:write"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.1 — the ID of a test channel the bot has been invited to")
	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t))
	defer cancel()

	pacer := &slack.ChannelPacer{}
	requestHash := "live_smoke_" + time.Now().UTC().Format("20060102T150405")

	if err := pacer.Wait(ctx, channel); err != nil {
		t.Fatalf("pace the first post: %v", err)
	}
	posted, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.postMessage",
		Token:     token,
		// The same minter and the same two arguments slack_approval_post.go hands it in production; the detail
		// stands in for publicationDisplay's resolved destination, which needs a run to have one.
		Body: slack.ApprovalMessage(channel, "", "E19 T2 live smoke — nothing is actually approved by this", requestHash),
	}, slack.PostOptions{})
	if err != nil {
		// A REAL refusal of our Block Kit is exactly what this test exists to surface, so it fails loudly.
		t.Fatalf("real Slack refused the approval message we construct: %v — the Block Kit surface in slack.ApprovalMessage is not one Slack accepts", err)
	}
	if posted.MessageTS == "" {
		t.Fatal("chat.postMessage returned no ts; without it there is no handle to repair the visible message")
	}
	t.Logf("posted the approval message: ts=%s attempts=%d repaired=%t", posted.MessageTS, posted.Attempts, posted.Repaired)

	// The SLK-006 repair: the SAME message is edited, never duplicated. The pacer holds the documented
	// per-channel rate between the two calls.
	if err := pacer.Wait(ctx, channel); err != nil {
		t.Fatalf("pace the repair: %v", err)
	}
	repaired, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.update",
		Token:     token,
		Body:      slack.UpdateMessage(channel, posted.MessageTS, "Approved: E19 T2 live smoke (repaired in place)", ""),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("chat.update could not repair the visible message: %v", err)
	}
	if repaired.MessageTS != posted.MessageTS {
		t.Fatalf("the repair landed on ts %q, want the ORIGINAL %q — an edit that moves the message is a second post, not a repair (SLK-006)",
			repaired.MessageTS, posted.MessageTS)
	}
}
