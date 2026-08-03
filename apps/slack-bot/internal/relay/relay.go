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

	"github.com/palgroup/palai/adapters/integrations/slack"
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
	// UpdateTask renders one step of the run in the message's task timeline — a card, not body prose.
	// Calling it again with the same Task.ID advances THAT card rather than drawing a second one.
	UpdateTask(ctx context.Context, channel, ts string, task slack.Task) error
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
//
// THE TRAILING NEWLINE IS THE SAME SEPARATOR emitStatus guarantees, and it is needed HERE for a reason that
// path does not cover: this text is chat.startStream's markdown_text, so it never passes through emitStatus
// at all, and the first model_step.delta.v1 appends directly onto it. Without it a plain question — no tool
// calls, which per E08 is EVERY real single-step run — comes back reading `Working…Projede toplam 7 Swift
// dosyası var`. That is the `doneProjede` defect on the one path that had no status line to blame.
const startingStatus = "Working…\n"

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

	st := &openStream{deps: deps, channel: channel, threadTS: threadTS, ts: ts,
		taskTitles: map[string]string{}, taskDetailed: map[string]bool{}}
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

	// taskTitles is the title each card was drawn with, keyed by tool_call_id, because the events that
	// advance a card do not all carry the tool's name (see updateTask). It is per-Run state and dies with
	// the stream — a tool_call_id is unique to one run's ledger, so nothing here outlives its usefulness.
	taskTitles map[string]string
	// taskDetailed records which cards have already been given a detail, because a card's `details` field
	// APPENDS rather than replaces — so the second and later details need a separator in front of them.
	taskDetailed map[string]bool
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
	case "tool_call.progress.v1":
		// Progress keeps the step IN PROGRESS and replaces what the card shows underneath it, so a long
		// tool reads as one step whose detail advances rather than a growing pile of lines.
		//
		// A notification with nothing to say updates NOTHING. Re-sending the card with an empty detail
		// would blank whatever the previous update put there, so a messageless progress event — which is
		// most of them, since the field is advisory — would erase the line it was supposed to refine.
		if detail := toolProgressDetail(event.Data); detail != "" {
			o.updateTask(ctx, event.Data, "in_progress", detail)
		}
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
	default:
		if status, ok := toolCallStatus[event.Type]; ok {
			// NO DETAIL, EVER, FROM A tool_call LIFECYCLE EVENT — the card gets a status and a title and
			// nothing else. This started as the raw tool name and the owner read it twice as noise: a card
			// saying "Reading files" with `palai.workspace.file` under it repeats itself in machine words,
			// and on a tool that reports no progress (shell) that was the card's ONLY line — so the one
			// thing a reader wants (which command) was absent and its internal name was there instead.
			//
			// THE NAME IS NOT LOST, because toolTitle falls back to it for a tool this tree has no phrase
			// for: an unmapped name still shows, as the title, exactly where it is the most informative
			// thing available. What is gone is repeating it when a human phrase already said it.
			//
			// AND THE ARGUMENT CANNOT REPLACE IT. Measured 2026-08-04 against the live journal
			// (`SELECT type, payload FROM events WHERE type LIKE 'tool_call.%'`), a frame is exactly
			// {run_id, tool_name, tool_call_id[, replay_class]} — the command, the path and the query are
			// NOT in it. They sit behind GET /v1/responses/{id}/tool-calls, and a per-call fetch to
			// decorate a card would buy latency and a failure surface for a caption. So the honest card
			// carries what the events actually know, and progress fills the line when a tool sends any.
			o.updateTask(ctx, event.Data, status, "")
		}
	}
}

// toolCallStatus maps a tool call's journalled events onto the task vocabulary blocks.go renders
// (adapters/integrations/slack, TaskStatus: open|in_progress|done|failed|canceled → Slack's
// pending|in_progress|complete|error).
//
// IT IS EXHAUSTIVE OVER THE STATE MACHINE'S TERMINALS ON PURPOSE, and that is the SLK-P2 lesson applied to
// a card instead of a stream: a terminal this map omits leaves its step drawn `in_progress` forever, so the
// finished message shows a run still working. Every state packages/state-machines/tool_call.go can reach
// from `executing` is listed, plus the three an `uncertain` call is reconciled to.
//
// MEASURED WRITERS (2026-08-03, `grep -rn '"tool_call\.<state>\.v1"' --include='*.go' .` minus tests and
// the transition table itself): executing 3, completed 2, canceled 2, reconciled_completed 2, uncertain 1,
// reconciled_not_applied 1, manual_resolution 1 — and **failed 0**. `tool_call.failed.v1` is declared by the
// transition table and journalled by NOTHING today: a tool that returns an error does so as a RESULT FIELD
// on a `completed` call (the tree records this as "a tool error is an ANSWER"), so the failure a reader
// actually sees arrives as text, not as this event. It is mapped anyway because the transition exists and
// the cost of mapping an event that never fires is nothing, while the cost of the reverse — the event
// starting to fire into a map that does not know it — is a card that spins forever.
var toolCallStatus = map[string]string{
	"tool_call.executing.v1":              "in_progress",
	"tool_call.completed.v1":              "done",
	"tool_call.reconciled_completed.v1":   "done",
	"tool_call.failed.v1":                 "failed",
	"tool_call.canceled.v1":               "canceled",
	"tool_call.uncertain.v1":              "failed",
	"tool_call.reconciled_not_applied.v1": "failed",
	"tool_call.manual_resolution.v1":      "failed",
}

// updateTask draws (or advances) the ONE card this tool call owns.
//
// THE ID IS THE WHOLE MECHANISM: Slack advances a card when a later task_update repeats its task_id and
// draws a second card when it does not, so the id must be something the executing event and its terminal
// already SHARE rather than a counter this package invents. `tool_call_id` is that thing — both writers
// hardcode it into the payload (packages/coordinator/orchestration.go BeginToolCall and
// apps/control-plane/internal/execution/tool_dispatch.go, measured 2026-08-03) — and it is stable across
// the pair by construction, because it IS the ledger row's primary key.
//
// A card is skipped rather than guessed when there is no id to hang it on: an empty task_id is not a card
// Slack can update, and two steps sharing one would overwrite each other, which is worse than the step not
// being drawn. Nothing is lost that a reader needed — the answer text is untouched either way.
func (o *openStream) updateTask(ctx context.Context, data map[string]any, status, detail string) {
	id := dataString(data, "tool_call_id")
	if id == "" {
		return
	}
	// THE TITLE IS REMEMBERED PER CARD BECAUSE NOT EVERY EVENT CARRIES ONE. A progress notification's
	// payload is {tool_call_id, progress, total, message} and nothing else
	// (packages/coordinator/mcp_progress.go AppendToolProgress, measured 2026-08-03) — no tool_name. Deriving
	// the title from each event alone would therefore RENAME a live step from "Reading files" to the
	// no-name fallback the moment it reported progress, which is worse than never having titled it.
	title := o.taskTitles[id]
	if named := dataString(data, "tool_name"); named != "" || title == "" {
		title = toolTitle(data)
		o.taskTitles[id] = title
	}
	// A CARD'S DETAIL APPENDS, so consecutive details need the separator for the same reason the message
	// body does — without it the live workspace returned
	// `palai.workspace.fileSwiftUIListApp.swift (3/7)`, which is `doneProjede` again, on the card. The
	// FIRST detail takes no newline: it has nothing to be separated from.
	if detail != "" {
		if o.taskDetailed[id] {
			detail = "\n" + detail
		}
		o.taskDetailed[id] = true
	}
	// The error is DROPPED, and that is a decision rather than an oversight: a card is decoration over an
	// answer delivered by an entirely separate path, so a failed update must not fail the relay, and it must
	// not enter openStream.pending either — that buffer is the model's words retrying, and a step's card
	// arriving late on the back of a text append would be worse than the step simply not advancing. The
	// answer still arrives whatever this call does.
	_ = o.deps.Slack.UpdateTask(ctx, o.channel, o.ts, slack.Task{
		ID: id, Title: title, Status: status, Detail: detail,
	})
}

// toolTitles turns a tool's registered name into something a human reads. The names are a closed set the
// deployment registers, measured from the tree on 2026-08-03 with
// `grep -rhoE '"palai\.[a-z_.]+"' --include='*.go' packages/ apps/ adapters/ | sort -u` → 16, of which the
// 15 real ones are below (`palai.conformance.math.add` is a conformance fixture, not a thing to narrate).
//
// AN UNLISTED NAME FALLS BACK TO ITSELF rather than to a guess: an MCP server contributes tool names this
// tree has never seen, and `palai.workspace.file` read as "reading files" is a kindness only because
// somebody checked what it does. Inventing a title from an unknown identifier is how a card ends up
// confidently describing the wrong action.
var toolTitles = map[string]string{
	"palai.workspace.file":             "Reading files",
	"palai.workspace.shell":            "Running a command",
	"palai.workspace.commit":           "Committing changes",
	"palai.workspace.background_kill":  "Stopping a background task",
	"palai.workspace.show_media":       "Showing a screenshot",
	"palai.publish.pull_request":       "Opening a pull request",
	"palai.publish.merge_pull_request": "Merging the pull request",
	"palai.publish.push":               "Pushing the branch",
	"palai.task":                       "Updating the task list",
	"palai.todo":                       "Updating the to-do list",
	"palai.web.search":                 "Searching the web",
	"palai.slack.search":               "Searching Slack",
	"palai.research.fetch":             "Fetching a page",
	"palai.knowledge.retrieve":         "Searching the knowledge base",
	"palai.fs.write":                   "Writing a file",
}

// toolTitle is the card's headline, and for a mapped tool it is the ONLY thing the card says — nothing
// repeats it in machine words underneath (see updateTask's caller).
//
// THE FALLBACK IS WHERE A RAW NAME STILL APPEARS, and that is the whole reason dropping it elsewhere loses
// nothing: a tool with no phrase in toolTitles is titled with its own name. So the name is shown exactly
// when it is the most informative thing available, and omitted exactly when a human sentence already said
// the same thing.
func toolTitle(data map[string]any) string {
	name := toolName(data)
	if title, ok := toolTitles[name]; ok {
		return title
	}
	return name
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

// toolName is the raw registered name off a tool_call.* event (packages/coordinator/orchestration.go
// BeginToolCall — "the tool's name rides the frame", E30 T2). tool_name is expected on every tool_call
// event; a server that omits it (or a malformed event) still gets a readable, generic word rather than a
// card titled with the empty string, which Slack rejects.
func toolName(data map[string]any) string {
	if name := dataString(data, "tool_name"); name != "" {
		return name
	}
	return "a tool"
}

// toolProgressDetail renders an MCP tools/call progress notification (packages/coordinator/mcp_progress.go
// AppendToolProgress) as the line under a step's card. It is advisory and often carries nothing worth
// showing: a message with no total is still shown, but a notification with neither leaves the card's detail
// EMPTY — which, because it keeps the same task_id, leaves the step exactly as it was rather than blanking
// what the previous update put there.
func toolProgressDetail(data map[string]any) string {
	message := dataString(data, "message")
	if message == "" {
		return ""
	}
	if total, ok := dataFloat64(data, "total"); ok && total > 0 {
		progress, _ := dataFloat64(data, "progress")
		return fmt.Sprintf("%s (%.0f/%.0f)", message, progress, total)
	}
	return message
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
