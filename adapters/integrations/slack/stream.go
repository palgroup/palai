package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// The AGENT STREAMING wire (E20 T1, spec §36). Three calls, and each one rides PostMessage — the SAME sender
// the approval message and the return leg already use, which owns the 429 + Retry-After discipline. That is
// deliberate and it is the plan's §2 rule: there is exactly ONE retry owner in this package, and a second
// layer here would retry against a budget that is not the workspace's.
//
// HONEST CEILING, at the top because it governs every line below and is the single most over-claimable thing
// in this epic: THIS IS NOT TOKEN STREAMING. The run journal has no delta event — its finest-grained output
// event is model_step.completed.v1 — and E08's rule (no tools are exposed to a real provider) makes a real
// Slack run SINGLE-STEP. So a real run has exactly ONE thing to stream. What these calls buy is a live status
// while the model is working and a message that appears when the step lands rather than after the terminal
// transaction. Rich multi-step streaming is driven by a FAKE ENGINE and is named that way wherever it
// appears. Token-level streaming needs an engine-side model_step.delta.v1; it is a separate epic.
//
// THE TIER ASYMMETRY IS THE REAL CAPACITY LIMIT (S8): chat.startStream is Tier 2 (20+/min) while
// chat.appendStream is Tier 4 (100+/min), so STARTING streams — i.e. concurrent runs — is five times scarcer
// than appending to them. That is why the follower caps concurrent streams and skips rather than queues.

// MaxStreamMarkdown is how much markdown_text one streaming call carries.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.startStream/ and
// https://docs.slack.dev/reference/methods/chat.appendStream/ (both checked 2026-07-27) — "Limit this field
// to 12,000 characters." The limit is CHARACTERS, so the cut below counts runes, not bytes.
const MaxStreamMarkdown = 12000

// ErrNoStreamRecipient is a channel stream missing the recipient (or the thread) Slack requires. FAIL-CLOSED:
// the call is not made at all, so the caller falls back to a plain post rather than spending one of the
// twenty per-minute startStream requests on something Slack answers with missing_recipient_user_id.
var ErrNoStreamRecipient = errors.New("slack: a channel stream needs a thread and both recipient ids")

// StreamStart is the argument set chat.startStream takes for a CHANNEL thread — the only kind of stream this
// tree opens, because every Slack run it admits is born from a channel mention.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.startStream/ (checked 2026-07-27) — required
// `channel` and `thread_ts`; optional `markdown_text`, `chunks`, `recipient_user_id`, `recipient_team_id`,
// `task_display_mode`; scope `chat:write`; Tier 2 (20+ per minute); success is {ok, channel, ts}.
//
// S9, and it is the reason RecipientUserID/RecipientTeamID are on this struct rather than optional: the page
// says of both, verbatim, "Required when streaming to channels." Both values already exist on slack.Event
// (UserID, TeamID), so honoring this costs no extra read.
//
// WHAT THIS DOES NOT CLAIM (S9's unconfirmed half): the documentation does not say what OTHER members of the
// channel see while a stream is in progress. The claim made anywhere in this tree is only "the requester is
// shown the stream, and the terminal message stays for everyone" — never "everyone watches it live".
type StreamStart struct {
	Channel         string
	ThreadTS        string
	RecipientUserID string
	RecipientTeamID string
	MarkdownText    string
}

// StartStream opens a streaming message in a channel thread and returns the ts Slack assigned it — the handle
// every later append and the final stop address. A missing recipient or thread refuses without calling.
func StartStream(ctx context.Context, doer Doer, apiBase string, token []byte, req StreamStart) (string, error) {
	if req.ThreadTS == "" || req.RecipientUserID == "" || req.RecipientTeamID == "" {
		return "", fmt.Errorf("%w (channel %s)", ErrNoStreamRecipient, req.Channel)
	}
	body, _ := json.Marshal(map[string]any{
		"channel":           req.Channel,
		"thread_ts":         req.ThreadTS,
		"recipient_user_id": req.RecipientUserID,
		"recipient_team_id": req.RecipientTeamID,
		"markdown_text":     TruncateMarkdown(req.MarkdownText),
	})
	res, err := PostMessage(ctx, doer, PostRequest{MethodURL: apiBase + "/chat.startStream", Token: token, Body: body}, PostOptions{})
	if err != nil {
		return "", err
	}
	return res.MessageTS, nil
}

// AppendStream adds to an open stream.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.appendStream/ (checked 2026-07-27) — required
// `channel`, `ts`, `markdown_text`; optional `chunks`; scope `chat:write`; Tier 4 (100+ per minute). Notable
// error codes: `stopped_by_user`, `message_not_in_streaming_state`, `message_not_owned_by_app`.
//
// THERE IS NO `blocks` PARAMETER HERE AND THAT IS A DECISION, NOT AN OMISSION (S12). Two Slack pages
// CONTRADICT each other: https://docs.slack.dev/ai/developing-agents/ (checked 2026-07-27) says "Blocks may
// be used in the chat.stopStream method, but not the chat.startStream or chat.appendStream method", while the
// chat.appendStream reference above documents a `blocks` chunk type and the invalid_blocks /
// msg_blocks_too_many errors. An unresolved vendor contradiction is not a design freedom: this takes the
// CONSERVATIVE reading, so blocks travel only on stopStream. If a live measurement shows appends accept
// blocks, adding them is a widening with evidence behind it — not a correction.
func AppendStream(ctx context.Context, doer Doer, apiBase string, token []byte, channel, ts, markdownText string) error {
	body, _ := json.Marshal(map[string]any{
		"channel":       channel,
		"ts":            ts,
		"markdown_text": TruncateMarkdown(markdownText),
	})
	_, err := PostMessage(ctx, doer, PostRequest{MethodURL: apiBase + "/chat.appendStream", Token: token, Body: body}, PostOptions{})
	return err
}

// StopStream closes a stream with its final text and, optionally, the rendered blocks that sit under it.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.stopStream/ (checked 2026-07-27) — required
// `channel` and `ts`; optional `markdown_text` (12,000 characters), `blocks`, `chunks`, `metadata`; scope
// `chat:write`; Tier 2 (20+ per minute). Blocks "will be rendered after" the markdown_text.
//
// blocks is raw JSON so this stays PURE WIRE: what the array contains is E20 T4's renderer's business, and
// the security rule that governs it lives there — an actionable element (`actions`, `button`, `action_id`, …)
// may only ever be minted by interactions.go. nil omits the field rather than sending a null.
func StopStream(ctx context.Context, doer Doer, apiBase string, token []byte, channel, ts, markdownText string, blocks json.RawMessage) error {
	payload := map[string]any{
		"channel":       channel,
		"ts":            ts,
		"markdown_text": TruncateMarkdown(markdownText),
	}
	if len(blocks) > 0 {
		payload["blocks"] = blocks
	}
	body, _ := json.Marshal(payload)
	_, err := PostMessage(ctx, doer, PostRequest{MethodURL: apiBase + "/chat.stopStream", Token: token, Body: body}, PostOptions{})
	return err
}

// TruncateMarkdown holds text to the documented 12,000-CHARACTER budget, and the cut is never silent: the
// result ends with an ellipsis, because a reader who cannot tell a truncated answer from a complete one has
// been told something false. Applied inside each call above too, so a caller that forgets it cannot hand
// Slack an over-long field.
func TruncateMarkdown(text string) string {
	text = NeutralizeBroadcasts(text)
	runes := []rune(text)
	if len(runes) <= MaxStreamMarkdown {
		return text
	}
	return string(runes[:MaxStreamMarkdown-1]) + "…"
}

// NeutralizeBroadcasts defuses the mention tokens in text a MODEL had a hand in.
//
// A broadcast is not a click, but it IS an action: `<!channel>` notifies everyone in the channel, `<!here>`
// everyone present, `<!subteam^S…>` a user group, and `<@U…>` one person. None of them are things a run's
// output gets to decide, and all of them are one prompt injection away — the same reasoning as the epic's
// carrying rule (a model must not be able to mint an element that acts on people), applied to the text path
// rather than the block path.
//
// SO WHERE IT IS APPLIED MATTERS: here, in the ONE function every streaming call routes its text through, and
// in ThreadReply, which is the other. That is every path by which model-influenced text reaches a workspace
// today. E20 T4's renderer owns the BLOCK half of the same rule.
//
// It escapes the opening angle bracket rather than deleting the token, so the reader still sees what the
// model wrote — `<!channel>` renders as literal text instead of vanishing. Silent removal would be its own
// small lie.
//
// SOURCE, and the honest label: https://docs.slack.dev/messaging/formatting-message-text/ (checked
// 2026-07-27) documents the mention syntax and says to escape `<`, `>` and `&` in text that is not meant to
// be markup. It does NOT say an app must neutralize its own model's output — nothing does. This is OUR
// defence, not a spec requirement, and it is labelled that way rather than dressed up as a vendor rule.
func NeutralizeBroadcasts(text string) string {
	if !strings.Contains(text, "<!") && !strings.Contains(text, "<@") {
		return text
	}
	text = strings.ReplaceAll(text, "<!", "&lt;!")
	return strings.ReplaceAll(text, "<@", "&lt;@")
}
