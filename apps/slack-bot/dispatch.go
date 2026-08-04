package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	"github.com/palgroup/palai/apps/slack-bot/internal/relay"
	palai "github.com/palgroup/palai/sdks/go"
)

// dispatcher is where a Socket Mode envelope becomes a Palai turn — the join this task exists to make.
// Everything either side of it already existed and neither end had a caller: the loop
// (internal/socket) hands over payload bytes and knows nothing about Palai; relay.HandleEvent and
// relay.OnButton take a decoded event and know nothing about a WebSocket. This type is the only thing
// that knows both, and it is deliberately thin — mapping is the adapter's, meaning is the relay's.
//
// THE PAYLOAD IS BYTE-IDENTICAL TO WHAT AN HTTP CALLBACK WOULD HAVE CARRIED, which is what makes the two
// mappings below the SAME ones the control plane's HTTP routes drive. Nothing about identity moves when
// the transport does.
type dispatcher struct {
	inbound   relay.InboundDeps
	approvals relay.ApprovalDeps
	// botUserID is passed to MapEvent for the self-loop guard and for stripping a mention's own `<@U…>`
	// out of the text. It may be empty; MapEvent still drops anything carrying a bot_id, which is what
	// this app's own posts carry.
	botUserID string
	logf      func(string, ...any)
}

func (d *dispatcher) log(format string, args ...any) {
	if d.logf != nil {
		d.logf(format, args...)
		return
	}
	log.Printf(format, args...)
}

// OnEventsAPI turns one events_api envelope into a turn.
//
// EVERY REFUSAL BELOW IS A DIFFERENT FACT AND THEY ARE NOT COLLAPSED. ErrIgnored is this app's own
// message coming back at it (the loop guard) and is the most common outcome in a busy channel, so it is
// silent — a log line per bot post is noise that hides the lines that matter. ErrNoRun is an agent-panel
// surface, handled and deliberately birthing nothing. A mapping error is a malformed envelope. And a
// HandleEvent failure is the loud one: the envelope was acknowledged before this ran, so Slack will never
// redeliver it, and the human who typed the message will get no answer and no error — that has to be
// visible in the log or it is invisible everywhere.
func (d *dispatcher) OnEventsAPI(ctx context.Context, payload json.RawMessage) {
	// retry=false: Socket Mode publishes no redelivery hint on the envelope (the HTTP transport's
	// X-Slack-Retry-Num has no documented counterpart here).
	ev, err := slack.MapEvent(payload, d.botUserID, false)
	switch {
	case errors.Is(err, slack.ErrIgnored):
		return
	case errors.Is(err, slack.ErrNoRun):
		d.log("slack-bot: acknowledged panel surface event %q (tab=%q channel=%s) and birthed no run", ev.Type, ev.Tab, ev.ChannelID)
		return
	case err != nil:
		d.log("slack-bot: a malformed events_api envelope arrived: %v", err)
		return
	}
	if err := relay.HandleEvent(ctx, d.inbound, ev); err != nil {
		d.log("slack-bot: THE TURN WAS LOST — event=%s channel=%s thread=%s. The envelope was already acknowledged, so Slack will not redeliver it and the person who wrote the message will see no answer at all: %v",
			ev.SourceEventID, ev.ChannelID, ev.ThreadTS, err)
	}
}

// OnInteractive turns one interactive envelope into an approval decision.
//
// The HTTP route has to url-decode a `payload` form parameter before it can map one; here the envelope's
// payload IS that JSON, so the same mapping runs over the same shape.
//
// ErrApproverNotAllowed is logged like any other failure and NOT answered in Slack. That is a real
// ceiling and not an oversight: repairing the message would take a chat.update, and this bridge repairs
// only messages whose decision it actually made (relay.OnButton's SLK-006 single repair). So an
// unlisted clicker sees nothing happen — the click is refused before the control plane is ever called,
// which is the property that matters, but the silence is worth knowing about.
func (d *dispatcher) OnInteractive(ctx context.Context, payload json.RawMessage) {
	intent, err := slack.MapInteractiveApproval(payload)
	switch {
	case errors.Is(err, slack.ErrNotApproval):
		// NOT A DECISION IS NOT NOTHING. The third button slack.ToolApprovalMessage mints, "Show
		// arguments", lands here by construction — MapInteractiveApproval's switch matches only
		// approve/deny, which is asserted from both directions in the adapter's own tests — and it is the
		// one non-decision this bot acts on. Anything that is neither goes on being ignored.
		d.onShowArguments(ctx, payload)
		return
	case err != nil:
		d.log("slack-bot: a malformed interactive envelope arrived: %v", err)
		return
	}
	if err := relay.OnButton(ctx, d.approvals, intent); err != nil {
		d.log("slack-bot: the %s click by %s in %s was not applied: %v", intent.Decision, intent.UserID, intent.ChannelID, err)
	}
}

// onShowArguments serves a click on the third button: open the modal listing that call's arguments in
// full, inside the three seconds the trigger lives.
//
// AN UNMAPPABLE PAYLOAD IS SILENT AND EVERYTHING ELSE IS LOGGED. slack.ErrNotShowArguments covers every
// interaction that is neither a decision nor this button — a future element, a payload from another app's
// message — and a log line per one of those is noise. A click that DID map and then did not open is worth
// a line, because a human pressed something and watched nothing happen, and this process is the only place
// that fact exists: the modal is not repaired in Slack (relay.OnShowArguments's own ceiling), so silence
// here is silence everywhere.
func (d *dispatcher) onShowArguments(ctx context.Context, payload json.RawMessage) {
	click, err := slack.MapShowArgumentsClick(payload)
	if err != nil {
		return
	}
	if err := relay.OnShowArguments(ctx, d.approvals, click); err != nil {
		d.log("slack-bot: the Show-arguments click by %s in %s opened nothing — they pressed the button and "+
			"saw no modal, and nothing in Slack tells them why: %v", click.UserID, click.ChannelID, err)
	}
}

// onApprovalRequested is the ApprovalHook every relay.Run gets: a gated call parked the run, so post the
// message a human decides from, in the thread that run is already rendering into.
//
// A FAILURE HERE IS THE QUIETEST ONE THIS PROCESS HAS, which is why it is worded the way it is. The run
// is already parked server-side waiting for an answer; if this post does not land, nobody is ever asked,
// nothing times out visibly, and the thread simply stops. relay.Run cannot act on it (see
// relay.ApprovalHook), so saying it out loud here is the whole of the response.
//
// EXCEPT FOR ONE OUTCOME THAT IS NOT A FAILURE AT ALL, and it became routine the day recovery shipped.
// A recovered run REPLAYS its journal (relay/resume.go), so every approval.requested.v1 it already handled
// arrives here a second time — and by then the approval is usually DECIDED, which drops it out of
// GET /v1/approvals and makes findPendingApproval answer ErrApprovalNotFound. Logging the sentence above
// for that says three things that are all false: that a run is parked, that a human is owed a question,
// and that the thread will stop. Measured live 2026-08-04, on a thread whose approval had been decided
// minutes earlier.
//
// It is reported anyway, because the same error also covers the case where the row has not committed
// visibly yet — which IS a question nobody can see. What changes is that the line says which of the two a
// reader is looking at instead of asserting the worse one.
func (d *dispatcher) onApprovalRequested(ctx context.Context, channel, threadTS string, ev palai.Event) {
	err := relay.OnApprovalRequested(ctx, d.approvals, channel, threadTS, ev)
	switch {
	case err == nil:
	case errors.Is(err, relay.ErrApprovalNotFound):
		d.log("slack-bot: no open approval matches session=%s channel=%s thread=%s — it was almost certainly decided already (a recovered run replays the events it handled before it was interrupted). Nothing was posted, and nothing is owed unless this approval is genuinely still pending: %v",
			ev.SessionID, channel, threadTS, err)
	default:
		d.log("slack-bot: NOBODY WAS ASKED — session=%s channel=%s thread=%s. The run is parked waiting for an approval and the message carrying the buttons did not post, so the thread will simply stop: %v",
			ev.SessionID, channel, threadTS, err)
	}
}

// onRunFailed is the RunFailedHook every relay.Run this process starts reports through.
//
// IT IS THE ONLY PLACE THIS FAILURE CAN BE SEEN. HandleEvent returned as soon as Palai accepted the turn, so
// the run drains on a goroutine with no caller above it; the error relay.Run hands back has nowhere else to
// go. Before this existed it went nowhere at all, and the resulting episode is the reason the line is worded
// the way it is: the run itself SUCCEEDS on the control plane and its answer is journalled, so an operator
// reading only the Palai side sees a healthy session and no reason to look further. What failed is the
// rendering, and the thread is where the loss shows.
func (d *dispatcher) onRunFailed(err error, sessionID, channel, threadTS string) {
	d.log("slack-bot: THE ANSWER NEVER REACHED THE THREAD — session=%s channel=%s thread=%s. The turn was accepted and the run may well have completed on the control plane; what failed is relaying it into Slack, so the thread keeps whatever it last showed: %v",
		sessionID, channel, threadTS, err)
}
