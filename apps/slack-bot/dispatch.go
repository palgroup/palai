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
		// Includes the third button slack.ToolApprovalMessage mints, "Show arguments": it opens a modal
		// through views.open, which this bot does not wire (see approvals.go's file doc). It decides
		// nothing, so ignoring it is an incomplete affordance rather than a gap.
		d.log("slack-bot: ignored a non-approval interaction")
		return
	case err != nil:
		d.log("slack-bot: a malformed interactive envelope arrived: %v", err)
		return
	}
	if err := relay.OnButton(ctx, d.approvals, intent); err != nil {
		d.log("slack-bot: the %s click by %s in %s was not applied: %v", intent.Decision, intent.UserID, intent.ChannelID, err)
	}
}

// onApprovalRequested is the ApprovalHook every relay.Run gets: a gated call parked the run, so post the
// message a human decides from, in the thread that run is already rendering into.
//
// A FAILURE HERE IS THE QUIETEST ONE THIS PROCESS HAS, which is why it is worded the way it is. The run
// is already parked server-side waiting for an answer; if this post does not land, nobody is ever asked,
// nothing times out visibly, and the thread simply stops. relay.Run cannot act on it (see
// relay.ApprovalHook), so saying it out loud here is the whole of the response.
func (d *dispatcher) onApprovalRequested(ctx context.Context, channel, threadTS string, ev palai.Event) {
	if err := relay.OnApprovalRequested(ctx, d.approvals, channel, threadTS, ev); err != nil {
		d.log("slack-bot: NOBODY WAS ASKED — session=%s channel=%s thread=%s. The run is parked waiting for an approval and the message carrying the buttons did not post, so the thread will simply stop: %v",
			ev.SessionID, channel, threadTS, err)
	}
}
