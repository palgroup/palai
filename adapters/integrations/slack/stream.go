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
// in this epic: THIS IS STILL NOT TOKEN-LEVEL STREAMING, but the reason changed underneath it, and it is
// stated precisely below rather than as "now correct" — a comment that only says a claim was fixed carries
// no more information than the stale claim it replaced.
//
// STREAMING GRANULARITY. model_step.delta.v1 has had a production WRITER since 2026-08-02 (commit e3708ae6,
// "the declared streaming event finally has a writer": packages/coordinator/mcp_progress.go's
// AppendModelStepDelta, called from apps/control-plane/internal/execution/model_dispatch.go's onDelta). Text
// arriving at AppendStream below can therefore be the model's real streamed output now, not only a status
// line — but what arrives is a COALESCED WINDOW, not a token: the dispatch-side sink
// (apps/control-plane/internal/execution/model_delta_sink.go) batches a provider's stream into 500ms windows
// before journalling one, matching the SSE endpoint's own journal-tail poll cadence
// (apps/control-plane/api/events.go's PollInterval, default 500ms — the two are the same number on purpose).
// A live channel that bypasses the journal and delivers per token is still absent, and it is NOT needed here:
// chat.appendStream is Tier 4, ~600ms between calls at its sustained rate (S8 below), a cadence the 500ms
// window already matches.
//
// E08's rule (no tools are exposed to a real provider) is a SEPARATE axis this did not touch: it still makes
// a real Slack run SINGLE-STEP, so a real run still has exactly one model step to stream — what changed is
// how much of THAT step's text arrives live, not how many steps there are. Rich multi-step streaming is still
// driven by a FAKE ENGINE and is named that way wherever it appears.
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
	// TaskDisplayMode opens the stream in CHUNK MODE and says how its task cards are drawn. Empty opens an
	// ordinary text stream. THIS IS A WHOLE-STREAM DECISION, NOT A FORMATTING HINT — see StartStream.
	TaskDisplayMode string
}

// TaskDisplayModePlan GROUPS a run's steps into ONE container instead of leaving a card per step loose in
// the thread — which is what makes a finished answer calm rather than preceded by a column of ticks.
//
// CONTRACT: https://docs.slack.dev/reference/methods/chat.startStream/ (checked 2026-08-04) — task_display_mode
// is `timeline` (the default), `plan` or `dense`, and it is an argument of chat.startStream ONLY: a stream
// cannot change how its steps are drawn once it is open. Of `plan` the page says it "displays all tasks
// together, with the first task's placement determining the placement of the rest"; and
// https://slack.dev/slack-thinking-steps-ai-agents/ (checked 2026-08-04) says of the modes that they are
// "collapsed by default, so they don't overwhelm the conversation".
//
// THIS TREE PREVIOUSLY CHOSE `timeline` AND GAVE A REASON THAT IS MEASURED FALSE. The reason was that "plan
// GROUPS STEPS DECLARED UP FRONT, and we do not have them" — a Palai run discovers its tool calls as the model
// makes them, so a plan was thought to require foreknowledge this bot cannot have. Driven against the live API
// (2026-08-04, workspace T0AMPM5JX8U) with the EXACT incremental sequence a real run produces — four
// `task_update` chunks arriving one at a time, none announced in advance — plan mode answers ok and keeps:
//
//	{"type":"plan","title":"Thinking completed","tasks":[{task_id,title,status} ×4]}
//
// So the grouping is built from the same per-step chunks `timeline` takes; nothing is declared up front and no
// foreknowledge is claimed. The old comment described the BLOCK-side plan (blocks.go's TaskDisplayMode, which
// renders a list this tree really does hold all at once), not this one.
//
// `dense` IS NOT OFFERED BECAUSE IT MEASURED IDENTICAL TO `timeline`. Its documented job is to collapse
// "consecutive tool calls into a single summarized task card"; replayed with the same four consecutive calls it
// kept four separate `task_card` blocks, byte for byte what `timeline` kept. Whatever it does is not visible in
// the message the workspace stores, so choosing it would be choosing a difference this tree cannot show.
const TaskDisplayModePlan = "plan"

// StartStream opens a streaming message in a channel thread and returns the ts Slack assigned it — the handle
// every later append and the final stop address. A missing recipient or thread refuses without calling.
//
// IT ALSO FIXES THE STREAM'S MODE FOR LIFE, which is the single most surprising thing about this API and is
// MEASURED rather than documented (2026-08-04, workspace T0AMPM5JX8U — no Slack page states it):
//
//   - Opened with `markdown_text`, the stream is a TEXT stream. Every later chunk is refused with
//     `streaming_mode_mismatch`.
//   - Opened with `chunks`, it is a CHUNK stream. Every later `markdown_text` is refused the same way —
//     on chat.appendStream AND on chat.stopStream, so a chunk stream that tries to flush its last words as
//     text cannot even close, and a message left open renders as permanently "streaming" (SLK-P2).
//
// There is no mixing and no switching. TaskDisplayMode is therefore what CHOOSES the mode here rather than a
// display preference bolted onto an otherwise unchanged call: task cards can only exist in chunk mode, so
// asking for them and asking for chunk mode are one decision. A caller in chunk mode must use
// AppendStreamChunks and StopStreamChunks for everything, including the model's own prose.
func StartStream(ctx context.Context, doer Doer, apiBase string, token []byte, req StreamStart) (string, error) {
	if req.ThreadTS == "" || req.RecipientUserID == "" || req.RecipientTeamID == "" {
		return "", fmt.Errorf("%w (channel %s)", ErrNoStreamRecipient, req.Channel)
	}
	payload := map[string]any{
		"channel":           req.Channel,
		"thread_ts":         req.ThreadTS,
		"recipient_user_id": req.RecipientUserID,
		"recipient_team_id": req.RecipientTeamID,
	}
	if req.TaskDisplayMode != "" {
		payload["task_display_mode"] = req.TaskDisplayMode
		// The opening text rides a CHUNK, because sending it as markdown_text would open a text stream and
		// every task card this mode exists for would then be refused.
		//
		// AN EMPTY OPENING SENDS NO CHUNK AT ALL, which is what lets a caller open a stream that shows nothing
		// until the run itself has something to show. MEASURED (2026-08-04): chat.startStream with
		// task_display_mode and NO `chunks` key is answered ok, and the message it opens keeps `text:""` with
		// no blocks. That matters because the alternative is not free — whatever opens the body STAYS in the
		// body, above the answer, for as long as the message exists (see relay.Run, which used to open with
		// "Working…" and left every finished answer prefixed by it).
		if req.MarkdownText != "" {
			payload["chunks"] = []map[string]any{MarkdownChunk(req.MarkdownText)}
		}
	} else {
		payload["markdown_text"] = TruncateMarkdown(req.MarkdownText)
	}
	body, _ := json.Marshal(payload)
	res, err := PostMessage(ctx, doer, PostRequest{MethodURL: apiBase + "/chat.startStream", Token: token, Body: body}, PostOptions{})
	if err != nil {
		return "", err
	}
	return res.MessageTS, nil
}

// AppendStreamChunks appends CHUNKS rather than body text — the call that puts a run's steps in Slack's
// task timeline instead of writing them into the answer as prose.
//
// WHY IT IS A SEPARATE FUNCTION rather than a field on AppendStream: the two are not variations of one
// call, they belong to the two different STREAM MODES StartStream documents. `markdown_text` is refused
// outright on a chunk stream, so a caller does not choose between them per call — it chose once, at start.
//
// MEASURED (2026-08-04): a chunks-only chat.appendStream is answered ok:true. The reference lists
// `markdown_text` among the REQUIRED arguments, which read literally would make this call impossible; it is
// not required here, and sending it would in fact be the thing that fails.
//
// The chunks are built by TaskUpdateChunk (blocks.go), which is where the defusing and the status mapping
// live. This function is pure wire and does not inspect them.
func AppendStreamChunks(ctx context.Context, doer Doer, apiBase string, token []byte, channel, ts string, chunks []map[string]any) error {
	if len(chunks) == 0 {
		return nil
	}
	body, _ := json.Marshal(map[string]any{
		"channel": channel,
		"ts":      ts,
		"chunks":  chunks,
	})
	_, err := PostMessage(ctx, doer, PostRequest{MethodURL: apiBase + "/chat.appendStream", Token: token, Body: body}, PostOptions{})
	return err
}

// StopStreamChunks closes a CHUNK-MODE stream.
//
// IT EXISTS BECAUSE THE MODE SPLIT REACHES THE CLOSING CALL TOO, which is the half of the measurement that
// actually bites: chat.stopStream with `markdown_text` on a chunk stream is refused with
// `streaming_mode_mismatch` (measured 2026-08-04), so a chunk stream closed the ordinary way does not close
// AT ALL — and a stream that never closes renders as permanently "streaming" in Slack, which is SLK-P2, the
// exact defect the relay's whole deferred-stop design exists to prevent.
//
// Closing with no final text is normal and sends no chunks: everything has usually already been appended.
func StopStreamChunks(ctx context.Context, doer Doer, apiBase string, token []byte, channel, ts string, chunks []map[string]any) error {
	payload := map[string]any{"channel": channel, "ts": ts}
	if len(chunks) > 0 {
		payload["chunks"] = chunks
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
