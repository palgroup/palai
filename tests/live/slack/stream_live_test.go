//go:build live

// The Slack STREAMING + STATUS live smoke (E20 T1). Written now, runs UNCHANGED the moment the owner supplies
// credentials, and SKIPS — naming the missing variable and the §0 handover row that supplies it — rather than
// failing, so a partial handover reports partial-green.
//
// What these settle that no fixture can, and it is more than usual here because E20 builds on Slack's
// NEWEST surfaces (streaming shipped 2025-10, agent_view 2026-07), which means the fakes standing in for them
// are this repository's youngest:
//
//	S3 USED TO STAND HERE: that assistant.threads.setStatus really does work on `chat:write` alone. It was
//	CONFIRMED true against a real workspace, and then the capability it confirmed was superseded — the working
//	indicator is now the chat.startStream container headline (relay.go's setHeadline / PlanUpdateChunk), and
//	slack.SetStatus had no production caller left to prove anything for. It was deleted in the 2026-08-05
//	dead-export cleanup along with this test; S3's confirmed answer is recorded here rather than erased,
//	because "it once worked" is still a fact even once nothing calls it.
//
//	S16(c) — that a Socket-Mode-only app can call the streaming Web API methods at all. The Web API is
//	outbound, so it SHOULD, but no page says it, and the assumption is load-bearing for a deployment with no
//	public URL. TestLiveSlackStreamingWorksFromASocketModeApp asks it directly.
//
//	S9's unconfirmed half — that recipient_user_id + recipient_team_id are accepted the way we send them. A
//	fake peer answers {"ok":true} to any argument set; a missing_recipient_user_id can only come from Slack.
//
//	S16(a) and S16(b) — what happens to a stream nobody stops, and whether chat.update can edit a message
//	that was streamed. Both are MEASURED and LOGGED rather than asserted, because we do not know the answer:
//	asserting an expectation we invented would turn an open question into a fake one. S16(b) in particular
//	decides whether the approval repair could ever address a streamed message, which is why slack_reply.go
//	deliberately does not move last_bot_message_ts onto one today.
package live

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/palgroup/palai/adapters/integrations/slack"
)

// openThread posts the message a stream will hang under and returns its ts. A channel stream needs a
// thread_ts (it is REQUIRED on chat.startStream), and in production that thread root is the human's own
// mention — here it is a message saying what this is.
func openThread(ctx context.Context, t *testing.T, token []byte, channel, text string) string {
	t.Helper()
	posted, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.postMessage", Token: token,
		Body: slack.ThreadReply(channel, "", text, ""),
	}, slack.PostOptions{})
	if err != nil {
		t.Fatalf("could not open a thread to stream into: %v", err)
	}
	return posted.MessageTS
}

// TestLiveSlackStreamingWorksFromASocketModeApp is S16(c)'s assertion and the whole streaming round trip: open,
// append twice, close. Any refusal is a real one — our fake peer accepts every argument set we send it, so
// this is the only place an invalid_arguments or a missing_recipient_user_id can appear.
func TestLiveSlackStreamingWorksFromASocketModeApp(t *testing.T) {
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.1 — App → OAuth & Permissions → Bot User OAuth Token; scope chat:write"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.1 — the ID of a test channel the bot has been invited to")
	team := need(t, "SLACK_TEAM_ID", "§0.3 — the workspace's team id (T…), unchanged from E19")
	// S9's recipient, read HERE rather than through a helper so the live-leg inventory's env-var scan reads
	// what this test actually needs. SLACK_APPROVER_IDS is §0.1's allow-list; its first entry is a real member
	// of the workspace, and SLACK_TEST_USER_ID overrides it for an operator who would rather not use theirs.
	user := os.Getenv("SLACK_TEST_USER_ID")
	if user == "" {
		user = strings.TrimSpace(strings.Split(need(t, "SLACK_APPROVER_IDS",
			"§0.1 — the approver allow-list; its first entry is the stream's recipient. SLACK_TEST_USER_ID overrides it"), ",")[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t))
	defer cancel()

	thread := openThread(ctx, t, token, channel, "E20 T1 live smoke — the streaming round trip")
	pacer := &slack.ChannelPacer{}

	// TaskDisplayMode is set here where it was not originally: production NEVER opens a text-mode stream
	// (relay/inbound.go's channelSlackStream.StartStream always sends TaskDisplayModePlan), and the mode is
	// fixed for the STREAM'S WHOLE LIFE (StartStream's own doc) — a text-mode open followed by a chunk-mode
	// append/close would earn `streaming_mode_mismatch` on the very first append, not a mixture. Opening the
	// mode production actually uses is what makes the round trip below representative of it.
	ts, err := slack.StartStream(ctx, http.DefaultClient, slackAPIBase(), token, slack.StreamStart{
		Channel: channel, ThreadTS: thread,
		RecipientUserID: user, RecipientTeamID: team,
		MarkdownText:    "_Working on it…_",
		TaskDisplayMode: slack.TaskDisplayModePlan,
	})
	if err != nil {
		t.Fatalf("real Slack refused chat.startStream: %v — this is S16(c) (can a Socket-Mode-only app stream?) and S9 (are the recipient ids the ones it wants?) answering NO", err)
	}
	if ts == "" {
		t.Fatal("chat.startStream returned no ts; without it nothing can append to or close the stream")
	}
	t.Logf("S16(c) CONFIRMED: a Socket-Mode-only app opened a stream over the Web API (channel %s ts %s)", channel, ts)

	// Chunks, not markdown_text: a chunk-mode stream (see TaskDisplayMode above) refuses markdown_text on
	// every call, appendStream included. AppendStream/StopStream — this smoke's original text-mode calls —
	// were deleted in the 2026-08-05 dead-export cleanup once production stopped opening that mode at all;
	// this keeps the round trip proving the mode production actually uses.
	for _, line := range []string{"• running a tool…", "• tool finished"} {
		if err := pacer.Wait(ctx, channel); err != nil {
			t.Fatalf("pace the append: %v", err)
		}
		if err := slack.AppendStreamChunks(ctx, http.DefaultClient, slackAPIBase(), token, channel, ts,
			[]map[string]any{slack.MarkdownChunk(line)}); err != nil {
			t.Fatalf("chat.appendStream was refused: %v", err)
		}
	}

	if err := pacer.Wait(ctx, channel); err != nil {
		t.Fatalf("pace the close: %v", err)
	}
	if err := slack.StopStreamChunks(ctx, http.DefaultClient, slackAPIBase(), token, channel, ts,
		[]map[string]any{slack.MarkdownChunk("E20 T1 live smoke complete. This message was streamed, not posted whole.")}); err != nil {
		t.Fatalf("chat.stopStream was refused: %v — the stream would be left open forever", err)
	}

	// S16(b), MEASURED not asserted: can chat.update edit a message that was streamed? The approval repair
	// (slack_decision.go) edits messages by ts, and if this works then last_bot_message_ts could safely point
	// at a streamed message. Today slack_reply.go deliberately does not move it onto one. Either answer is
	// information; neither fails this test, because we have no documented expectation to hold Slack to.
	if err := pacer.Wait(ctx, channel); err != nil {
		t.Fatalf("pace the S16(b) probe: %v", err)
	}
	_, err = slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.update", Token: token,
		Body: slack.UpdateMessage(channel, ts, "E20 T1 live smoke — S16(b) probe: chat.update on a streamed message", ""),
	}, slack.PostOptions{})
	if err != nil {
		t.Logf("S16(b) MEASURED: chat.update REFUSED a streamed message (%v). The approval repair must never be pointed at one — which is what slack_reply.go already does.", err)
	} else {
		t.Logf("S16(b) MEASURED: chat.update EDITED a streamed message. Moving last_bot_message_ts onto a streamed ts becomes safe; that is a widening, and it needs its own change.")
	}
}

// TestLiveSlackUnstoppedStreamIsMeasured is S16(a): what happens to a stream nobody closes. It is the exact
// shape of this task's restart ceiling — a control plane that dies mid-run leaves precisely this behind — so
// what the operator sees here is what a real restart will look like.
//
// It ASSERTS only what we do know (an append still works after a wait, i.e. the message stays in streaming
// state for at least that long) and LOGS the ts so the operator can look at the channel and say what it
// renders as. The message is left open ON PURPOSE; the log says so, so nobody reads it as a bug.
func TestLiveSlackUnstoppedStreamIsMeasured(t *testing.T) {
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.1 — App → OAuth & Permissions → Bot User OAuth Token; scope chat:write"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.1 — the ID of a test channel the bot has been invited to")
	team := need(t, "SLACK_TEAM_ID", "§0.3 — the workspace's team id (T…), unchanged from E19")
	// S9's recipient, read HERE rather than through a helper so the live-leg inventory's env-var scan reads
	// what this test actually needs. SLACK_APPROVER_IDS is §0.1's allow-list; its first entry is a real member
	// of the workspace, and SLACK_TEST_USER_ID overrides it for an operator who would rather not use theirs.
	user := os.Getenv("SLACK_TEST_USER_ID")
	if user == "" {
		user = strings.TrimSpace(strings.Split(need(t, "SLACK_APPROVER_IDS",
			"§0.1 — the approver allow-list; its first entry is the stream's recipient. SLACK_TEST_USER_ID overrides it"), ",")[0])
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t))
	defer cancel()

	thread := openThread(ctx, t, token, channel,
		"E20 T1 live smoke — S16(a): this thread's next message is DELIBERATELY left streaming, to show what a control-plane restart looks like")
	// TaskDisplayMode set for the same reason the S16(c) test above sets it: production only ever opens a
	// chunk-mode stream, and the mode is fixed for the stream's whole life.
	ts, err := slack.StartStream(ctx, http.DefaultClient, slackAPIBase(), token, slack.StreamStart{
		Channel: channel, ThreadTS: thread,
		RecipientUserID: user, RecipientTeamID: team,
		MarkdownText:    "_Working on it…_ (this stream is intentionally never closed)",
		TaskDisplayMode: slack.TaskDisplayModePlan,
	})
	if err != nil {
		t.Fatalf("chat.startStream: %v", err)
	}

	time.Sleep(30 * time.Second)
	appendErr := slack.AppendStreamChunks(ctx, http.DefaultClient, slackAPIBase(), token, channel, ts,
		[]map[string]any{slack.MarkdownChunk("• still open 30s later")})
	if appendErr != nil {
		if code := slack.APIErrorCode(appendErr); code != "" {
			t.Logf("S16(a) MEASURED: after 30s an unstopped stream refuses appends with %q — Slack closes or expires it on its own, which BOUNDS the restart ceiling", code)
			return
		}
		t.Fatalf("append to the unstopped stream failed for a non-API reason: %v", appendErr)
	}
	t.Logf("S16(a) MEASURED: an unstopped stream is STILL in streaming state after 30s (channel %s ts %s). "+
		"That is what a control-plane restart leaves behind: a message stuck mid-stream, with the answer arriving as a NEW message. "+
		"Look at the channel and record how it renders. If it is ugly enough to matter, that is the trigger for the durable stream state "+
		"(migration 000042, owner approval) named in slack_stream.go.", channel, ts)
}

// TestLiveSlackStreamRefusesWithoutARecipient turns S9 from a sentence into a receipt: the page says the
// recipient ids are "Required when streaming to channels", and this asks Slack to prove it. Our client refuses
// before calling (fail-closed), so the raw call is made here deliberately, bypassing StartStream.
//
// If Slack ACCEPTS it, the fail-closed guard is stricter than the contract — which is a widening we could
// take, not a bug. Either way the answer is logged rather than guessed.
func TestLiveSlackStreamRefusesWithoutARecipient(t *testing.T) {
	token := []byte(need(t, "SLACK_BOT_TOKEN", "§0.1 — App → OAuth & Permissions → Bot User OAuth Token; scope chat:write"))
	channel := need(t, "SLACK_TEST_CHANNEL", "§0.1 — the ID of a test channel the bot has been invited to")
	ctx, cancel := context.WithTimeout(context.Background(), liveWindow(t))
	defer cancel()

	thread := openThread(ctx, t, token, channel, "E20 T1 live smoke — S9: a channel stream with no recipient")
	body, _ := json.Marshal(map[string]any{
		"channel": channel, "thread_ts": thread, "markdown_text": "this should be refused",
	})
	_, err := slack.PostMessage(ctx, http.DefaultClient, slack.PostRequest{
		MethodURL: slackAPIBase() + "/chat.startStream", Token: token, Body: body,
	}, slack.PostOptions{})
	switch code := slack.APIErrorCode(err); {
	case code == "missing_recipient_user_id" || code == "missing_recipient_team_id":
		t.Logf("S9 CONFIRMED against a real workspace: a channel stream without a recipient is refused with %q, which is exactly what StartStream refuses to send", code)
	case err != nil:
		t.Logf("S9 MEASURED: the recipient-less call failed with %v (not one of the two documented missing_recipient_* codes)", err)
	default:
		t.Log("S9 WIDENED: real Slack ACCEPTED a channel stream with no recipient ids. The fail-closed guard in StartStream is stricter than the contract — safe, but it could be relaxed deliberately.")
	}
}
