// Package relay is the loop that turns one Palai session's event stream into a live Slack message —
// the heart of the bot (2026-08-03 plan, Task 7). Given a session already running and a Slack thread
// already chosen, Run opens one chat.startStream message, forwards what the session journals into it as
// it arrives, and closes it the moment the run ends — in EVERY way a run can end, because a stream this
// package forgets to close renders as permanently "streaming" in Slack (the tree records this as
// SLK-P2).
//
// SCOPE: this is the relay core only. It does not open the Socket Mode connection (Task 9), does not
// decide which session a Slack thread maps to (Task 8), and does not gate a tool call on human approval
// (Task 10). Deps is the seam those tasks plug a real event source and a real Slack client into.
package relay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	palai "github.com/palgroup/palai/sdks/go"
)

// EventStream is the pull side of an already-open session event stream — exactly the shape
// *palai.SessionEventStream has (Sessions.Events): a real caller passes one straight through, a test
// passes a fixed sequence. Run reads it to a run terminal event or to its first error, and closes it
// exactly once either way.
type EventStream interface {
	Next() (palai.Event, error)
	Close() error
}

// Slack is the outbound half Run drives — the three chat.*Stream calls
// adapters/integrations/slack already implements, narrowed to what a channel-thread stream needs here.
// It deliberately does NOT take StreamStart's recipient fields (S9, adapters/integrations/slack/stream.go):
// those come from the Slack event that opened the thread, which only the caller wiring Deps (Task 9)
// has — a concrete implementation closes over them instead of this package carrying fields it has no
// way to fill.
type Slack interface {
	// StartStream opens the message and returns the ts every later call addresses it by.
	StartStream(ctx context.Context, channel, threadTS, markdownText string) (ts string, err error)
	// AppendStream adds markdownText to the open message; it is APPENDED, not a replacement.
	AppendStream(ctx context.Context, channel, ts, markdownText string) error
	// StopStream closes the message, appending markdownText (if any) one last time.
	StopStream(ctx context.Context, channel, ts, markdownText string) error
}

// ApprovalHook is what Run does with an approval.requested.v1 event: hand it to the approval bridge
// (approvals.go's OnApprovalRequested) for the thread this relay is rendering into. It exists because
// the bridge takes a channel and a thread ts that "nothing in a Palai event names" (OnApprovalRequested's
// own doc) — and Run is the one place that holds both alongside the event.
//
// IT RETURNS NOTHING, and the reason is not that failures do not matter — it is that Run cannot act on
// one. By the time this fires the run is already PARKED server-side waiting for a human, so there is
// nothing to abort and nothing to retry into; a failed post means a question nobody can see, which the
// caller's implementation is the right place to say out loud. Production (apps/slack-bot/main.go) logs it
// as exactly that.
type ApprovalHook func(ctx context.Context, channel, threadTS string, ev palai.Event)

// Deps is Run's whole seam: a session's events, the Slack stream to render them into, and where an
// approval request goes. Every field is required — Run refuses rather than doing half a job with a nil
// one, and a nil OnApproval in particular would be a run that parks on a human who is never asked.
type Deps struct {
	Events     EventStream
	Slack      Slack
	OnApproval ApprovalHook
}

// startingStatus is what the Slack message shows the instant a relay begins, before anything has
// journaled: the wait between a run being born and its first model step is otherwise a silent gap.
const startingStatus = "Working…"

// runTerminalEvents is Run's own copy of the run terminal set. It MIRRORS terminalEventTypes in
// apps/control-plane/api/events.go — the SSE endpoint's own closing set — rather than importing it,
// because that map is unexported there. A run terminal added to one and not the other is a real drift
// risk, catchable only by re-measuring both sides, not by the compiler.
var runTerminalEvents = map[string]bool{
	"run.completed.v1":       true,
	"run.failed.v1":          true,
	"run.canceled.v1":        true,
	"run.timed_out.v1":       true,
	"run.budget_exceeded.v1": true,
}

// Run drains sessionID's event stream into the Slack thread at channel/threadTS: one chat.startStream
// message, opened before the first event is even read, kept alive by chat.appendStream as
// model_step.delta.v1 and tool_call.* events arrive, and closed by chat.stopStream the moment a run
// terminal event lands — or the moment ANYTHING ELSE ends the loop (a read error, a panic), because the
// alternative is a Slack message stuck "streaming" forever. That guarantee is why the stop call lives in
// one deferred function covering the whole loop rather than at each terminal branch: a branch this
// package forgets to list can still never leak an open stream.
func Run(ctx context.Context, deps Deps, sessionID, channel, threadTS string) (err error) {
	if deps.Events == nil || deps.Slack == nil || deps.OnApproval == nil {
		return fmt.Errorf("relay: Deps needs Events, Slack and OnApproval (session %s)", sessionID)
	}
	defer deps.Events.Close()

	ts, startErr := deps.Slack.StartStream(ctx, channel, threadTS, startingStatus)
	if startErr != nil {
		return fmt.Errorf("relay: start stream for session %s: %w", sessionID, startErr)
	}

	st := &openStream{deps: deps, channel: channel, threadTS: threadTS, ts: ts}
	defer st.stop(ctx, sessionID, &err)

	for {
		event, nextErr := deps.Events.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("relay: read session %s events: %w", sessionID, nextErr)
		}
		st.handle(ctx, event)
		if runTerminalEvents[event.Type] {
			return nil
		}
	}
}

// openStream holds the state ONE Run call accumulates between events: where it is writing and the text
// still owed to Slack.
type openStream struct {
	deps    Deps
	channel string
	// threadTS is the thread this relay renders into. The stream itself is addressed by ts (Slack's
	// answer to chat.startStream), so this is carried only for the approval message, which is a separate
	// chat.postMessage into the same thread rather than an append to the open stream.
	threadTS string
	ts       string

	// pending is text that has not been CONFIRMED delivered: either nothing has been sent for it yet,
	// or the last AppendStream carrying it failed. It rides along on every later send (see emit), and
	// is flushed as the final chat.stopStream call's markdown_text.
	//
	// THAT FINAL FLUSH IS ONE MORE ATTEMPT, NOT A GUARANTEE, and the two branches are not the same
	// outcome: if the closing StopStream call succeeds, pending's text reaches the message; if
	// StopStream ALSO fails, that text is genuinely lost from Slack's side — there is no retry after
	// this one and nothing durable behind it. The loss is not silent at the Go level (stop() still
	// turns a StopStream failure into Run's returned error via *outErr, so a caller can act on it), but
	// the Slack message itself will be missing the text either way.
	pending strings.Builder
}

// stop closes the stream exactly once, from Run's single deferred call, so every exit path — a run
// terminal, a read error, a panic unwinding through this defer — closes the same way.
//
// A recovered panic becomes Run's returned error instead of crashing the process a whole bot's worth of
// OTHER sessions shares. *outErr is only overwritten when Run was not already returning a real error, so
// a StopStream failure never masks one.
func (o *openStream) stop(ctx context.Context, sessionID string, outErr *error) {
	rec := recover()
	// context.WithoutCancel: ctx can already be Done() here — that is exactly how a read error or a
	// caller-initiated shutdown reaches this defer — but the Slack message this call closes outlives
	// that ctx by definition, so closing it must not be refused for the same reason the loop stopped.
	stopErr := o.deps.Slack.StopStream(context.WithoutCancel(ctx), o.channel, o.ts, o.pending.String())
	switch {
	case rec != nil:
		*outErr = fmt.Errorf("relay: panic relaying session %s: %v", sessionID, rec)
	case *outErr == nil && stopErr != nil:
		*outErr = fmt.Errorf("relay: stop stream for session %s: %w", sessionID, stopErr)
	}
}

// handle renders one event into the stream. Unknown or uninteresting types are silently skipped —
// API-009 delivers every type rather than filtering the journal, and a relay that does not render a
// type is simply not decorating it yet, not refusing it.
func (o *openStream) handle(ctx context.Context, event palai.Event) {
	switch event.Type {
	case "model_step.delta.v1":
		o.emit(ctx, dataString(event.Data, "text"))
	case "tool_call.executing.v1":
		o.emitStatus(ctx, fmt.Sprintf("\n\n▶️ Running `%s`…", toolLabel(event.Data)))
	case "tool_call.progress.v1":
		o.emitStatus(ctx, toolProgressLine(event.Data))
	case "tool_call.completed.v1":
		o.emitStatus(ctx, fmt.Sprintf("\n✅ `%s` done", toolLabel(event.Data)))
	case ApprovalRequestedEventType:
		// A GATED CALL PARKED THE RUN, and this is the only place the bot learns of it: the approval
		// bridge (approvals.go) posts the message a human decides from, and until this case existed
		// nothing in this process ever called it — so no approve/deny button was ever minted and
		// OnButton could not fire in production however correct it was.
		//
		// It is announced in the stream FIRST so the open message says why it went quiet: the run is now
		// waiting on a person, and a stream that simply stops reads as a hang.
		o.emitStatus(ctx, "\n\n⏸️ Waiting for an approval — see the message below.")
		o.deps.OnApproval(ctx, o.channel, o.threadTS, event)
	}
}

// emitStatus writes one STATUS line — a line this package composed, never the model's own words — and
// guarantees it ends with a newline.
//
// THE TRAILING NEWLINE IS NOT COSMETIC. chat.appendStream APPENDS ("This text is what will be appended to
// the message received so far"), so the next model_step.delta.v1 lands against the last byte written here
// with nothing between them. Every status line in this file was missing it, and on 2026-08-03 the owner's
// first real answer came back reading `✅ \`palai.workspace.file\` doneProjede toplam 7 Swift dosyası var`
// — the status line and the answer's first word fused into one word.
//
// It is a HELPER rather than a newline typed onto each literal above because the property is "no status
// line can abut what follows it", and a property spread across four string literals is one edit away from
// being three-quarters true. A line that renders to nothing (a messageless progress notification) still
// emits nothing.
func (o *openStream) emitStatus(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	o.emit(ctx, text)
}

// emit appends text to the open stream, with anything still pending from an earlier failed send (see
// openStream.pending) prepended ahead of it. A successful AppendStream clears pending; a failed one adds
// this text to what was already waiting, so the NEXT attempt — including the final chat.stopStream —
// tries the whole backlog again rather than only the newest chunk.
func (o *openStream) emit(ctx context.Context, text string) {
	combined := o.pending.String() + text
	if combined == "" {
		return
	}
	if err := o.deps.Slack.AppendStream(ctx, o.channel, o.ts, combined); err != nil {
		o.pending.Reset()
		o.pending.WriteString(combined)
		return
	}
	o.pending.Reset()
}

// toolLabel names a tool_call.executing.v1/completed.v1 event's tool (packages/coordinator/orchestration.go
// BeginToolCall — "the tool's name rides the frame", E30 T2). tool_name is expected on both; a server
// that omits it (or a malformed event) still gets a readable, generic line instead of dropping the call.
func toolLabel(data map[string]any) string {
	if name := dataString(data, "tool_name"); name != "" {
		return name
	}
	return "a tool"
}

// toolProgressLine renders an MCP tools/call progress notification (packages/coordinator/mcp_progress.go
// AppendToolProgress), which is advisory and often has nothing worth a line: a message with no total is
// still shown, but a call with neither is skipped rather than rendering a bare newline.
func toolProgressLine(data map[string]any) string {
	message := dataString(data, "message")
	if message == "" {
		return ""
	}
	if total, ok := dataFloat64(data, "total"); ok && total > 0 {
		progress, _ := dataFloat64(data, "progress")
		return fmt.Sprintf("\n   ↳ %s (%.0f/%.0f)", message, progress, total)
	}
	return fmt.Sprintf("\n   ↳ %s", message)
}

// dataString reads a string field out of an event's decoded payload, or "" if it is absent or not a
// string. Data is a plain map[string]any (API-009's open union), so a differently-typed or missing field
// must degrade rather than panic.
func dataString(data map[string]any, key string) string {
	s, _ := data[key].(string)
	return s
}

// dataFloat64 reads a numeric field the same way: encoding/json decodes every JSON number found in a
// map[string]any as float64, never int.
func dataFloat64(data map[string]any, key string) (float64, bool) {
	f, ok := data[key].(float64)
	return f, ok
}
