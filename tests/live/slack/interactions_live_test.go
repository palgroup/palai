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
//	D8 (plan §3.5): that a real interactivity POST is form-encoded with a single `payload` parameter, and
//	that the v0 signature over the RAW form bytes verifies. Every local proof signs the way we BELIEVE Slack
//	signs; this one is signed by Slack.
//
//	That chat.update REPAIRS the visible message rather than posting a second one — a fake peer records what
//	it was asked to do, while a real workspace shows what actually happened.
package live

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
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
		Body:      slack.ApprovalMessage(channel, "", "E19 T2 live smoke — nothing is actually approved by this", requestHash),
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
		Body:      slack.UpdateMessage(channel, posted.MessageTS, "Approved: E19 T2 live smoke (repaired in place)"),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("chat.update could not repair the visible message: %v", err)
	}
	if repaired.MessageTS != posted.MessageTS {
		t.Fatalf("the repair landed on ts %q, want the ORIGINAL %q — an edit that moves the message is a second post, not a repair (SLK-006)",
			repaired.MessageTS, posted.MessageTS)
	}
}

// TestLiveSlackButtonClickIsFormEncodedAndVerifies is the D8 settlement, and it is the one that needs the
// owner in the loop: a REAL person clicks a REAL button and this receiver observes what Slack actually POSTs.
//
// SETUP the operator does once (§0.1): App → Interactivity & Shortcuts ON, with the Request URL pointed at
// this receiver's /v1/slack/interactions (a public HTTPS URL — Socket Mode does NOT use this transport and
// carries no signature, which is precisely what is under test here). Then click the Approve button on the
// message this test posts.
func TestLiveSlackButtonClickIsFormEncodedAndVerifies(t *testing.T) {
	secret := []byte(need(t, "SLACK_SIGNING_SECRET", "§0.1 — App → Basic Information → App Credentials → Signing Secret"))
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.1 — App → OAuth & Permissions → Bot User OAuth Token; scope chat:write"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.1 — the ID of a test channel the bot has been invited to")

	addr := os.Getenv("PALAI_SLACK_LIVE_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8099"
	}
	requestHash := "live_click_" + time.Now().UTC().Format("20060102T150405")

	var (
		mu       sync.Mutex
		clicked  slack.ApprovalIntent
		rawForm  string
		got      = make(chan struct{})
		once     sync.Once
		verifErr error
	)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/slack/interactions", func(w http.ResponseWriter, r *http.Request) {
		// EXACTLY the shipped route's order, and the point of the whole test: the RAW bytes are read first,
		// verified first, and only then decoded.
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		if err := slack.VerifySignature(secret, r.Header.Get(slack.HeaderTimestamp), r.Header.Get(slack.HeaderSignature),
			body, time.Now(), slack.DefaultTolerance); err != nil {
			mu.Lock()
			verifErr = err
			mu.Unlock()
			w.WriteHeader(http.StatusUnauthorized)
			once.Do(func() { close(got) })
			return
		}
		payload, err := slack.ExtractInteractionPayload(body)
		if err != nil {
			mu.Lock()
			verifErr = err
			rawForm = string(body)
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			once.Do(func() { close(got) })
			return
		}
		intent, err := slack.MapInteractiveApproval(payload)
		if err != nil {
			w.WriteHeader(http.StatusOK) // some other interaction; keep waiting for ours
			return
		}
		mu.Lock()
		clicked, rawForm = intent, string(body)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		once.Do(func() { close(got) })
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.ListenAndServe() }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	})

	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t))
	defer cancel()
	posted, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.postMessage",
		Token:     token,
		Body:      slack.ApprovalMessage(channel, "", "E19 T2 live click — press Approve; nothing is actually approved", requestHash),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("post the clickable message: %v", err)
	}
	t.Logf("posted ts=%s — CLICK THE APPROVE BUTTON in %s (waiting up to %v)", posted.MessageTS, channel, liveWindow(t))

	select {
	case <-got:
	case <-ctx.Done():
		t.Skipf("no interaction arrived within %v — the Request URL is probably not pointed at %s/v1/slack/interactions (§0.1: interactivity needs a PUBLIC HTTPS URL; Socket Mode does not use this transport)",
			liveWindow(t), addr)
	}

	mu.Lock()
	defer mu.Unlock()
	if verifErr != nil {
		t.Fatalf("a REAL interactivity POST failed our verify/extract (%v); raw body began %q — this is a D8 finding about the contract, not a flake",
			verifErr, truncate(rawForm, 120))
	}
	// D8, settled against real Slack rather than against our belief about it.
	if !strings.HasPrefix(rawForm, "payload=") {
		t.Fatalf("real Slack POSTed a body beginning %q, not the documented `payload=<urlencoded JSON>` form — the receiver's body-shape assumption is wrong",
			truncate(rawForm, 120))
	}
	if json.Valid([]byte(rawForm)) {
		t.Fatalf("real Slack POSTed valid JSON as the raw body (%q) — the form-encoding contract this route is built on does not hold", truncate(rawForm, 120))
	}
	if clicked.RequestHash != requestHash {
		t.Fatalf("the click carried request hash %q, want the minted %q — the button value is what binds a decision to an exact operation (SLK-007)",
			clicked.RequestHash, requestHash)
	}
	if clicked.UserID == "" || clicked.ChannelID != channel {
		t.Fatalf("the click mapped to %+v, want a clicker id and channel %s", clicked, channel)
	}
	t.Logf("real click: user=%s channel=%s thread=%s action=%s", clicked.UserID, clicked.ChannelID, clicked.ThreadTS, clicked.ActionID)

	// And the visible message is repaired in place, closing the round trip the fixture can only simulate.
	repaired, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.update",
		Token:     token,
		Body:      slack.UpdateMessage(channel, posted.MessageTS, "Approved by <@"+clicked.UserID+"> (E19 T2 live click)"),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("repair the clicked message: %v", err)
	}
	if repaired.MessageTS != posted.MessageTS {
		t.Fatalf("the repair landed on %q, want the clicked message %q", repaired.MessageTS, posted.MessageTS)
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
