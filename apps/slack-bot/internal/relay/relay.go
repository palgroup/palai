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

// Deps is Run's whole seam: a session's events and the Slack stream to render them into. Both fields
// are required — Run refuses rather than doing half a job with a nil one.
type Deps struct {
	Events EventStream
	Slack  Slack
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
	if deps.Events == nil || deps.Slack == nil {
		return fmt.Errorf("relay: Deps needs both Events and Slack (session %s)", sessionID)
	}
	defer deps.Events.Close()

	ts, startErr := deps.Slack.StartStream(ctx, channel, threadTS, startingStatus)
	if startErr != nil {
		return fmt.Errorf("relay: start stream for session %s: %w", sessionID, startErr)
	}

	st := &openStream{deps: deps, channel: channel, ts: ts}
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
	ts      string

	// pending is text that has not been CONFIRMED delivered: either nothing has been sent for it yet,
	// or the last AppendStream carrying it failed. It rides along on every later send (see emit), and
	// is flushed as the final chat.stopStream call's markdown_text — so text Slack never confirmed
	// still reaches the message once, at the end, rather than being silently dropped.
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
		o.emit(ctx, fmt.Sprintf("\n\n▶️ Running `%s`…", toolLabel(event.Data)))
	case "tool_call.progress.v1":
		o.emit(ctx, toolProgressLine(event.Data))
	case "tool_call.completed.v1":
		o.emit(ctx, fmt.Sprintf("\n✅ `%s` done", toolLabel(event.Data)))
	}
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
