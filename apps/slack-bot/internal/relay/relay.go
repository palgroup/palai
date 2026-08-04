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
	// PostMessage posts a PLAIN message into the thread — not into the open stream, which is the whole
	// point of it: it is the only way left to reach a human once Slack has refused the stream for good
	// (see stopped_by_user below). It is REQUIRED rather than optional for that reason; an implementation
	// without it turns the one recoverable Slack failure this relay has into a lost answer.
	PostMessage(ctx context.Context, channel, threadTS, markdownText string) error
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

// thinkingTaskID is the id of the ONE card that stands for the model's OWN working time — the intervals
// where the agent is composing rather than running a tool.
//
// IT IS A LITERAL, AND IT CANNOT COLLIDE with a tool's card: every other id in this stream is a
// `tool_call_id`, which the ledger mints as `tcall_<hex>`. Repeating this id is what makes the model's
// thinking ONE pulsing row rather than one row per step — five model steps in the measured run below would
// otherwise have left five identical "Thinking" cards in the finished message.
const thinkingTaskID = "palai.thinking"

// thinkingTitle is the card's headline in every state it can be in. It is DELIBERATELY NOT REWRITTEN as the
// status changes: `title` REPLACES on a task_update, so a title that tracked the state would be a second,
// redundant encoding of the status pill Slack already draws — and one more string able to disagree with it.
const thinkingTitle = "Thinking"

// THE GAP THIS CARD EXISTS TO FILL, measured end to end on 2026-08-04 against a real run
// (ses_3b6c1cf9a3a23336c1ec68a1dcf2095e, the relay's own send log timestamped from the stream opening):
//
//	t+ 0.0s  startStream "Working…"
//	t+ 8.5s  tool 1 in_progress          <- the FIRST thing that ever appeared, 33% into the run
//	t+ 8.8s  tool 1 done                 <- in_progress lasted 300ms
//	t+12.0s  tool 2 in_progress          <- 3.2s of nothing before it
//	t+12.5s  tool 2 done
//	t+15.0s  tool 3 in_progress          <- 2.5s of nothing
//	t+15.3s  tool 3 done
//	t+18.5s  tool 4 in_progress          <- 3.2s of nothing
//	t+18.8s  tool 4 done
//	t+20.5s  the answer's first text
//	t+26.4s  stopStream
//
// So of 26 seconds, roughly 19 showed a reader NOTHING NEW, and the four moments that did show something
// were each visible for a third of a second. That is the whole of "çalışırken hiçbir düşünüyor hâli
// göremedim": the run was not quiet, the RENDERING was.
//
// The same journal says what to draw in those gaps, and it is not a guess: every one of them is bracketed by
// a `model_step.created.v1` / `model_step.completed.v1` pair. The run's 33 events are
// run.queued/provisioning/running, then FIVE model steps — four that decide on a tool and emit no text at
// all, and a fifth that writes the answer as 11 deltas — interleaved with the four tool calls.
//
// WHAT THIS IS NOT: the model's reasoning CONTENT. Nothing in this product journals that; `model_step.*` and
// `tool_call.*` are the whole vocabulary (`grep -rhoE '"(model_step|reasoning)[a-z_.]*\.v1"' --include='*.go'
// packages apps | sort -u` -> the four model_step types and no reasoning type at all, 2026-08-04). What is
// journalled is the INTERVAL — that the model started working and that it stopped — and a card that says only
// that is saying exactly what the tree knows.

// modelStepStatus maps the model's own step events onto the card vocabulary. `created` opens the interval and
// `completed` closes it, so across a multi-step run this one card pulses in_progress -> done once per step,
// and at any instant either it or a tool's card is live — never both, because the journal orders
// model_step.completed.v1 BEFORE the tool_call.executing.v1 it decided on.
//
// `model_step.interrupted.v1` is mapped even though the measured run never produced one: it is a declared
// terminal of the same step (the four types the grep above found), and an unmapped terminal is exactly how a
// card is left spinning in a finished message.
var modelStepStatus = map[string]string{
	"model_step.created.v1":     "in_progress",
	"model_step.completed.v1":   "done",
	"model_step.interrupted.v1": "failed",
}

// runTerminalThinking is how the thinking card is resolved by the RUN ending rather than by a step ending.
// It is the SLK-P2 rule applied to this card: a run that dies between `created` and `completed` — or before
// any model step at all, which is every provisioning failure — would otherwise leave it in_progress forever.
//
// AND SLACK ITSELF PUNISHES THAT, measured 2026-08-04: a task left in_progress when chat.stopStream lands is
// stored as `status:"error"` and drags the whole plan container's title to "Something went wrong". So an
// unresolved card does not merely look unfinished, it reports a failure that did not happen.
var runTerminalThinking = map[string]string{
	"run.completed.v1":       "done",
	"run.failed.v1":          "failed",
	"run.canceled.v1":        "canceled",
	"run.timed_out.v1":       "failed",
	"run.budget_exceeded.v1": "failed",
}

// isRunTerminal reports whether an event ends the run. The SET is runTerminalThinking's keys — ONE map
// serves both questions, because two maps listing the same five types is a drift the compiler cannot see and
// this file would have had to keep in step by hand.
//
// That set MIRRORS terminalEventTypes in apps/control-plane/api/events.go — the SSE endpoint's own closing
// set — rather than importing it, because that map is unexported there. A run terminal added to one and not
// the other is a real drift risk, catchable only by re-measuring both sides.
func isRunTerminal(eventType string) bool {
	_, ok := runTerminalThinking[eventType]
	return ok
}

// Run drains sessionID's event stream into the Slack thread at channel/threadTS: one chat.startStream
// message, opened before the first event is even read, kept alive by chat.appendStream as
// model_step.delta.v1 and tool_call.* events arrive, and closed by chat.stopStream the moment a run
// terminal event lands — or the moment ANYTHING ELSE ends the loop (a read error, a panic), because the
// alternative is a Slack message stuck "streaming" forever. That guarantee is why the stop call lives in
// one deferred function covering the whole loop rather than at each terminal branch: a branch this
// package forgets to list can still never leak an open stream.
//
// THE MESSAGE OPENS WITH AN EMPTY BODY AND A LIVE CARD, which is one decision with two halves.
//
// The body is empty because whatever opens it STAYS in it. This call used to pass "Working…\n", and that word
// is not a status — chat.appendStream appends, so it is the first line of the answer, permanently. The
// measured run's finished message read `Working… Projede toplam 281 Swift dosyası var.` and would have read
// that way for as long as the message existed. A progress word that cannot stop being shown is not progress.
//
// The card is drawn immediately because the run IS working from this instant and nothing else says so for
// several seconds (see thinkingTaskID's measurement: 8.5 of them, in the run that prompted this). Opening a
// stream in chunk mode with no chunks keeps a genuinely blank message, so without this the thread would show
// an empty bot reply during exactly the window the reader most needs to see something.
func Run(ctx context.Context, deps Deps, sessionID, channel, threadTS string) (err error) {
	if deps.Events == nil || deps.Slack == nil || deps.OnApproval == nil {
		return fmt.Errorf("relay: Deps needs Events, Slack and OnApproval (session %s)", sessionID)
	}
	defer deps.Events.Close()

	ts, startErr := deps.Slack.StartStream(ctx, channel, threadTS, "")
	if startErr != nil {
		return fmt.Errorf("relay: start stream for session %s: %w", sessionID, startErr)
	}

	st := &openStream{deps: deps, channel: channel, threadTS: threadTS, ts: ts,
		taskTitles: map[string]string{}, taskDetailed: map[string]bool{}}
	defer st.stop(ctx, sessionID, &err)
	st.setThinking(ctx, "in_progress")

	for {
		event, nextErr := deps.Events.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("relay: read session %s events: %w", sessionID, nextErr)
		}
		st.handle(ctx, event)
		if isRunTerminal(event.Type) {
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

	// thinkingStatus is what the thinking card currently shows, so a repeat is not re-sent. It matters
	// because the events that drive it are the most frequent ones a run has: a five-step run would spend
	// five extra chat.appendStream calls saying nothing changed, against a Tier 4 budget shared with the
	// answer's own text.
	thinkingStatus string

	// stopped records that the HUMAN pressed stop on Slack's streaming card — see userStopped. Once it is
	// set, nothing in this file writes to the stream again: Slack refuses every later append AND the
	// closing stop on a stopped stream, permanently, so a call made after this is a round trip that can
	// only fail.
	stopped bool
}

// THE HUMAN PRESSED STOP ON THE STREAMING CARD (S11, restored from the old in-process bridge's
// SlackStreamFollower.stoppedByUser — this relay shares no code with it and must not diverge from what it
// decided).
//
// WHAT SLACK DOES: chat.appendStream answers `stopped_by_user`, documented as "The streaming message was
// stopped by the user and no further appends are accepted" — and the refusal is PERMANENT and covers the
// closing chat.stopStream too, so a relay that keeps writing is a relay writing into a wall.
//
// WHAT THE RELAY USED TO DO WITH IT, and it is the defect this pair of constants closes: nothing. Every
// AppendStream failure took one branch (buffer into pending and try again on the next send), so after a stop
// every send failed, the closing StopStream failed, Run returned that error, and dispatch.go logged THE
// ANSWER NEVER REACHED THE THREAD. The model finished, its text was journalled server-side, and the person
// who pressed stop got NOTHING — which is not what pressing stop asks for. Stopping the live typing is a
// request about the RENDERING; it carries no authenticated actor, no approver identity and no command, so it
// is not run control (the old side's §2 invariant, unchanged here): the STREAM stops, the RUN does not.
//
// SO THE ANSWER MOVES TO A PLAIN MESSAGE, and that is the whole repair. Two things are said, at the two
// moments they are true:
const (
	// stoppedNotice is posted ONCE, the moment Slack first refuses, and only while the run is still going.
	// It exists because the human's model of what they just did is wrong in a way that costs them something:
	// they believe they cancelled the run, so a thread that then goes quiet reads as "cancelled", and a
	// question they type next STEERS the run they think they killed (HandleEvent's steer branch) instead of
	// asking a fresh one.
	//
	// It promises only what this relay can keep — that later text ARRIVES AS A MESSAGE — never that there
	// will be any. A run that had already said everything before the stop writes nothing more, and this
	// sentence stays true.
	stoppedNotice = "⏹️ Live updates stopped here. The run itself was not cancelled — it is still working, and whatever it writes from here will arrive as a message in this thread."
	// stoppedAnswerPrefix leads the plain message carrying what the stream never delivered. It says why the
	// text arrives detached from the card above it, because an answer that appears in a second message with
	// no explanation reads as the bot repeating itself.
	stoppedAnswerPrefix = "Live updates were stopped, so the rest of this answer is posted here:\n\n"
)

// userStopped records the stop and tells the thread, exactly once.
//
// THE NOTICE'S OWN FAILURE IS DROPPED, deliberately: what must not be lost is the ANSWER, and that failure
// IS reported (stop() turns it into Run's returned error, which dispatch.go logs). Failing the relay over an
// undelivered courtesy line would trade the thing that matters for the thing that does not.
func (o *openStream) userStopped(ctx context.Context, announce bool) {
	if o.stopped {
		return
	}
	o.stopped = true
	if announce {
		_ = o.deps.Slack.PostMessage(ctx, o.channel, o.threadTS, stoppedNotice)
	}
}

// setThinking advances the ONE card standing for the model's own working time, and sends nothing when the
// status is already what it would set — see openStream.thinkingStatus.
//
// The error is dropped for the same reason updateTask drops its own (a card is decoration over an answer
// delivered by a separate path), and the status is recorded BEFORE the call rather than after: a failed
// update must not make the next identical event retry, because the card's states are a sequence, and the
// right repair for a missed `in_progress` is the `done` that follows it, not a second `in_progress`.
func (o *openStream) setThinking(ctx context.Context, status string) {
	if o.thinkingStatus == status {
		return
	}
	o.thinkingStatus = status
	o.updateCard(ctx, slack.Task{ID: thinkingTaskID, Title: thinkingTitle, Status: status})
}

// stop closes the stream exactly once, from Run's single deferred call, so every exit path — a run
// terminal, a read error, a panic unwinding through this defer — closes the same way.
//
// A recovered panic becomes Run's returned error instead of crashing the process a whole bot's worth of
// OTHER sessions shares. *outErr is only overwritten when Run was not already returning a real error, so
// a delivery failure never masks one.
//
// THE CLOSE IS ALSO WHERE A STOP CAN BE DISCOVERED, and that case is not a rarity worth skipping: a human
// who presses stop while the model is composing the last of its answer produces NO further append, so the
// closing call is the first one Slack refuses — with the whole of pending on it. Handled as one shape with
// the mid-stream case: whatever the stream would not take is posted as a plain message.
func (o *openStream) stop(ctx context.Context, sessionID string, outErr *error) {
	rec := recover()
	// context.WithoutCancel: ctx can already be Done() here — that is exactly how a read error or a
	// caller-initiated shutdown reaches this defer — but the Slack message this call closes outlives
	// that ctx by definition, so closing it must not be refused for the same reason the loop stopped.
	closeCtx := context.WithoutCancel(ctx)
	var closeErr error
	if !o.stopped {
		if err := o.deps.Slack.StopStream(closeCtx, o.channel, o.ts, o.pending.String()); err != nil {
			if slack.APIErrorCode(err) == slack.CodeStoppedByUser {
				// announce=false: the notice says the run is still working, and by here it is not. The
				// plain message below carries its own explanation instead.
				o.userStopped(closeCtx, false)
			} else {
				closeErr = fmt.Errorf("relay: stop stream for session %s: %w", sessionID, err)
			}
		}
	}
	if o.stopped && o.pending.Len() > 0 {
		// THE ANSWER, ON THE ONLY PATH LEFT. An empty pending posts NOTHING: everything the model wrote had
		// already reached the card before the human stopped it, so there is nothing to repeat.
		if err := o.deps.Slack.PostMessage(closeCtx, o.channel, o.threadTS, stoppedAnswerPrefix+o.pending.String()); err != nil {
			closeErr = fmt.Errorf("relay: post session %s's answer after the human stopped the live stream: %w", sessionID, err)
		} else {
			o.pending.Reset()
		}
	}
	switch {
	case rec != nil:
		*outErr = fmt.Errorf("relay: panic relaying session %s: %v", sessionID, rec)
	case *outErr == nil && closeErr != nil:
		*outErr = closeErr
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
		// THE MODEL'S OWN WORKING TIME, which is most of a run's wall clock and used to render as nothing at
		// all. Taken BEFORE the tool arm below because the two vocabularies are disjoint and this one is
		// cheaper to decide; a step's card and a tool's card are never the same card (see thinkingTaskID).
		if status, ok := modelStepStatus[event.Type]; ok {
			o.setThinking(ctx, status)
			return
		}
		// A RUN TERMINAL RESOLVES THE THINKING CARD, and this is the only place that can: the card is opened
		// by Run before any event is read, so a run that ends without ever emitting a model step — a
		// provisioning failure, a cancel while queued — has nothing else to close it. Slack stores a card
		// still in_progress at stop as `error` and retitles the whole container "Something went wrong"
		// (measured 2026-08-04), so leaving it open would invent a failure on a run that merely had no steps.
		if status, ok := runTerminalThinking[event.Type]; ok {
			o.setThinking(ctx, status)
			return
		}
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
	// answer still arrives whatever this call does. updateCard owns the ONE exception (stopped_by_user).
	o.updateCard(ctx, slack.Task{ID: id, Title: title, Status: status, Detail: detail})
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
	"palai.workspace.glob":             "Finding files",
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
// A STOPPED STREAM SKIPS THE CALL ENTIRELY and keeps the text: Slack refuses every append on one forever,
// so an attempt is a round trip that can only fail, and the text it was carrying is delivered by stop() as a
// plain message instead (see the stoppedNotice block).
func (o *openStream) emit(ctx context.Context, text string) {
	combined := o.pending.String() + text
	if combined == "" {
		return
	}
	if !o.stopped {
		if err := o.deps.Slack.AppendStream(ctx, o.channel, o.ts, combined); err == nil {
			o.pending.Reset()
			return
		} else if slack.APIErrorCode(err) == slack.CodeStoppedByUser {
			o.userStopped(ctx, true)
		}
	}
	o.pending.Reset()
	o.pending.WriteString(combined)
}

// updateCard is the ONE place a card reaches Slack, and it exists so the two callers cannot disagree about
// the stopped stream. A card update is a chunk on the same stream as the answer's text, so it earns the same
// `stopped_by_user` refusal — and it can be the FIRST call to earn it, since a run whose model is composing
// draws cards between deltas. Learning it here rather than only in emit is what keeps a stopped stream from
// spending one wasted round trip per card for the rest of the run.
//
// The error is otherwise DROPPED, unchanged: a card is decoration over an answer delivered by a separate
// path (see updateTask's own note), and its text must never enter openStream.pending.
func (o *openStream) updateCard(ctx context.Context, task slack.Task) {
	if o.stopped {
		return
	}
	if err := o.deps.Slack.UpdateTask(ctx, o.channel, o.ts, task); err != nil &&
		slack.APIErrorCode(err) == slack.CodeStoppedByUser {
		o.userStopped(ctx, true)
	}
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
