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
	"time"

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
	// UpdatePlan writes the headline over the whole group of cards — the ONE line a reader sees while the
	// container is collapsed. Without it Slack writes that line itself and writes "Thinking" for the entire
	// length of the run (see slack.PlanUpdateChunk).
	UpdatePlan(ctx context.Context, channel, ts, title string) error
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

// Delivery is the DURABLE half of this relay, scoped to the one thread a Run is rendering into: what has
// already reached Slack, written somewhere that outlives this process.
//
// IT EXISTS BECAUSE A PROCESS LIFETIME WAS THE ONLY THING HOLDING AN ANSWER. Before it, how far a run had
// been rendered lived in a map in memory (inbound.go's inboundState) and nowhere else, so killing the bot
// mid-run did not delay that thread's answer, it DESTROYED it: no record named the thread, so nothing on
// restart could finish the job. The old in-process bridge this relay replaced had no such hole — it
// enqueued a run's reply inside the run's own terminal transaction (packages/coordinator/store.go
// EnqueueTerminalSlackReply) and a separate pump delivered it, so a dead pump meant a late answer. These
// three calls are how the relay gets that property back over a public API it does not own.
//
// THE THREE CALLS ARE NOT SYMMETRIC and the asymmetry is the whole contract:
//
//   - Opened names the Slack message, so another process can finish THAT message rather than posting a
//     second one (measured resumable across processes — see the stream_ts note in
//     migrations/0002_delivery_state.sql).
//   - Delivered advances a cursor that is BOTH the resume point and the de-duplication rule. Run calls it
//     only for events Slack has confirmed, so the worst a recovery can do is re-send an event that did
//     land — never skip one that did not.
//   - Closed says the run reached its terminal and the message is shut. Run calls it on NO other exit,
//     deliberately: a read error, a shutdown or a panic all leave the record pending, because "we stopped
//     reading" is exactly the state a restart has to pick up.
//
// Every error is returned rather than swallowed so the caller can say it out loud, but Run does not fail a
// run over one — see recordDelivered.
type Delivery interface {
	Opened(ctx context.Context, streamTS string) error
	Delivered(ctx context.Context, sequence int64) error
	Closed(ctx context.Context) error
}

// DeliveryFuncs adapts three closures to Delivery, for a caller that has the writes but no type to hang
// them on (inbound.go builds one per thread over the store).
type DeliveryFuncs struct {
	OpenedFunc    func(ctx context.Context, streamTS string) error
	DeliveredFunc func(ctx context.Context, sequence int64) error
	ClosedFunc    func(ctx context.Context) error
}

func (d DeliveryFuncs) Opened(ctx context.Context, streamTS string) error {
	if d.OpenedFunc == nil {
		return nil
	}
	return d.OpenedFunc(ctx, streamTS)
}

func (d DeliveryFuncs) Delivered(ctx context.Context, sequence int64) error {
	if d.DeliveredFunc == nil {
		return nil
	}
	return d.DeliveredFunc(ctx, sequence)
}

func (d DeliveryFuncs) Closed(ctx context.Context) error {
	if d.ClosedFunc == nil {
		return nil
	}
	return d.ClosedFunc(ctx)
}

// Deps is Run's whole seam: a session's events, the Slack stream to render them into, where an approval
// request goes, and the durable record of what has already been delivered. Every field is required — Run
// refuses rather than doing half a job with a nil one, and a nil OnApproval in particular would be a run
// that parks on a human who is never asked.
//
// Delivery is required for the same reason and not as bookkeeping: a Run with no durable record is exactly
// the relay this package used to be, whose answer a `kill` erased. Making it optional would put the one
// property this file exists to guarantee behind a caller remembering to ask for it.
type Deps struct {
	Events     EventStream
	Slack      Slack
	OnApproval ApprovalHook
	Delivery   Delivery

	// ResumeTS adopts a chat.startStream message that is ALREADY OPEN instead of opening a new one — the
	// recovery path (resume.go), where a previous process opened the message, wrote part of the answer into
	// it and died. Empty is the ordinary case: Run opens its own.
	//
	// It is safe across processes because a Slack stream is addressed by (channel, ts) and the bot token and
	// belongs to no connection — measured on 2026-08-04 by opening a stream in one curl invocation,
	// appending from a second and closing from a third, all ok, the text concatenating in order. So the
	// recovered half of an answer lands in the same message a reader is already looking at, and the card
	// that would otherwise have said "streaming" forever (SLK-P2) gets closed.
	ResumeTS string
}

// THE GAP THE HEADLINE EXISTS TO FILL, measured end to end on 2026-08-04 against a real run
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
// journalled is the INTERVAL — that the model started working and that it stopped — and a headline that says
// only that is saying exactly what the tree knows.
//
// IT USED TO BE A CARD AND IT IS NOW THE CONTAINER'S HEADLINE. A card called "Thinking" sat as the first row
// of the plan while Slack titled the plan itself "Thinking" — the same word, twice, in the two most
// prominent places on the message, and the row consumed one of the few lines a reader scans. The headline is
// where it belongs: it is the line that is visible while the container is collapsed, so the interval is
// reported exactly where a reader is already looking, and it costs no row at all.
const thinkingHeadline = "Thinking…"

// openingHeadline is the container's first line, written before any event has been read. The run IS working
// from that instant and nothing else says so for several seconds (8.5 of them in the run above).
const openingHeadline = "Working…"

// modelStepHeadline maps the model's own step events onto the container headline. `created` opens the
// interval; `completed` and `interrupted` are deliberately absent, because the event that follows one is
// always either the tool the step decided on or the run's own terminal — both of which write the headline
// themselves. Writing "idle" in between would spend a call to show a state that lasts milliseconds.
//
// `model_step.interrupted.v1` therefore needs no entry, and that is not the same omission that leaves a CARD
// spinning: a headline is REPLACED by whatever comes next, so nothing here can be left unresolved. The run
// terminal writes the last word in every case.
var modelStepHeadline = map[string]string{
	"model_step.created.v1": thinkingHeadline,
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
// THE STREAM IS ADOPTED RATHER THAN OPENED WHEN Deps.ResumeTS IS SET, and that branch is the difference
// between an answer arriving where a reader is looking and a second message appearing under a first one
// stuck at "Working…". A resumed stream also skips the opening headline: the message it is joining has been
// showing one for as long as the dead process lived, and re-announcing "Working…" over the top of whatever
// the run had already reported would erase the most recent thing a reader was told.
func Run(ctx context.Context, deps Deps, sessionID, channel, threadTS string) (err error) {
	if deps.Events == nil || deps.Slack == nil || deps.OnApproval == nil || deps.Delivery == nil {
		return fmt.Errorf("relay: Deps needs Events, Slack, OnApproval and Delivery (session %s)", sessionID)
	}
	defer deps.Events.Close()

	ts := deps.ResumeTS
	resumed := ts != ""
	if !resumed {
		var startErr error
		if ts, startErr = deps.Slack.StartStream(ctx, channel, threadTS, ""); startErr != nil {
			return fmt.Errorf("relay: start stream for session %s: %w", sessionID, startErr)
		}
		// RECORDED BEFORE THE FIRST EVENT IS READ, because the message exists from this instant and a
		// process killed one line later has to leave something behind that names it. A failure here is
		// reported and the run continues: the alternative is refusing to render an answer because we could
		// not write down where we were rendering it.
		if openErr := deps.Delivery.Opened(ctx, ts); openErr != nil {
			return fmt.Errorf("relay: record the open stream for session %s: %w", sessionID, openErr)
		}
	}

	st := &openStream{deps: deps, channel: channel, threadTS: threadTS, ts: ts,
		cards: newCards(), startedAt: time.Now()}
	defer st.stop(ctx, sessionID, &err)
	if !resumed {
		st.setHeadline(ctx, openingHeadline)
	}

	for {
		event, nextErr := deps.Events.Next()
		if nextErr != nil {
			if errors.Is(nextErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("relay: read session %s events: %w", sessionID, nextErr)
		}
		st.handle(ctx, event)
		st.delivered(ctx, int64(event.Sequence))
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

	// cards is the plan's steps and the grouping rule that decides how many there are (cards.go). It is
	// per-Run state and dies with the stream — a tool_call_id is unique to one run's ledger, so nothing
	// here outlives its usefulness.
	cards *openStreamCards

	// startedAt is when the stream opened, which is what the closing headline reports as the run's length.
	// It is measured from the OPEN rather than from the first event because that is when the person who
	// asked started waiting.
	startedAt time.Time

	// headline is what the container title currently says, so a repeat is not re-sent. It matters because
	// the events that drive it are the most frequent ones a run has: a thirty-three step run would spend
	// dozens of extra chat.appendStream calls saying nothing changed, against a Tier 4 budget shared with
	// the answer's own text.
	headline string

	// stopped records that the HUMAN pressed stop on Slack's streaming card — see userStopped. Once it is
	// set, nothing in this file writes to the stream again: Slack refuses every later append AND the
	// closing stop on a stopped stream, permanently, so a call made after this is a round trip that can
	// only fail.
	stopped bool

	// terminal records that a run terminal event was rendered (runEnded). It is what stop() consults to
	// decide whether this thread still owes somebody an answer: ONLY a terminal clears the durable record.
	// Every other way out of Run's loop — a read error, a cancelled context, a panic — leaves it false on
	// purpose, so a restart picks the thread up and finishes it.
	terminal bool

	// handled is the highest sequence handle() has been called with, delivered or not. It is what stop()
	// records once a flush finally lands: the text that was stuck in pending belonged to those events, so
	// the cursor may only move past them at the moment that text reaches Slack, never before.
	handled int64

	// deliveredSeq is the highest sequence already written through Delivery, so a repeat is not re-sent.
	// The durable write is a database round trip on the hot path of a stream; a run whose events carry no
	// new confirmed progress should not pay for one.
	deliveredSeq int64
}

// delivered advances the durable cursor — but ONLY for events Slack has actually taken.
//
// THE pending GUARD IS THE WHOLE CORRECTNESS ARGUMENT, not an optimisation. emit() buffers text into
// pending when an append fails and re-sends the whole backlog on the next attempt (see emit), so a
// non-empty pending means "some of what has been handled is NOT on screen". Recording a cursor past it
// would tell a recovering process that text was delivered which no reader has ever seen — and since the
// cursor is also the de-duplication rule, that text would then be skipped forever rather than retried.
// Erring the other way is cheap: a cursor left behind re-sends something that did land, which a reader
// sees once more, and only ever within one run's worth of events.
//
// A FAILED DURABLE WRITE DOES NOT FAIL THE RUN, and the error is dropped HERE rather than reported — not
// because it does not matter, but because this is the one place with nothing useful to say about it. The
// row keeps its previous cursor, so the whole cost of a failed write is a recovery that replays a few
// events it need not have. Reporting it up through Run would be worse than useless: dispatch.go renders a
// Run error as "THE ANSWER NEVER REACHED THE THREAD", which about a delivered answer is simply false. The
// caller that OWNS the write is the one that logs it, with the thread it belongs to — see the DeliveryFuncs
// inbound.go builds.
func (o *openStream) delivered(ctx context.Context, sequence int64) {
	if sequence > o.handled {
		o.handled = sequence
	}
	if o.pending.Len() > 0 || sequence <= o.deliveredSeq {
		return
	}
	o.deliveredSeq = sequence
	_ = o.deps.Delivery.Delivered(ctx, sequence)
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

// setHeadline writes the container's one visible line, and sends nothing when it already says this — see
// openStream.headline.
//
// The error is dropped for the same reason a card's is (both are decoration over an answer delivered by a
// separate path), and the text is recorded BEFORE the call rather than after: a failed write must not make
// the next identical event retry, because the headline is a sequence of states and the right repair for a
// missed one is the NEXT one, not a second attempt at the one that is already stale.
func (o *openStream) setHeadline(ctx context.Context, text string) {
	if text == "" || o.headline == text {
		return
	}
	o.headline = text
	if o.stopped {
		return
	}
	if err := o.deps.Slack.UpdatePlan(ctx, o.channel, o.ts, trimHeadline(text)); err != nil &&
		slack.APIErrorCode(err) == slack.CodeStoppedByUser {
		o.userStopped(ctx, true)
	}
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
	flushed := o.pending.Len() == 0
	if !o.stopped {
		if err := o.deps.Slack.StopStream(closeCtx, o.channel, o.ts, o.pending.String()); err != nil {
			if slack.APIErrorCode(err) == slack.CodeStoppedByUser {
				// announce=false: the notice says the run is still working, and by here it is not. The
				// plain message below carries its own explanation instead.
				o.userStopped(closeCtx, false)
			} else {
				closeErr = fmt.Errorf("relay: stop stream for session %s: %w", sessionID, err)
			}
		} else {
			flushed = true
		}
	}
	if o.stopped && o.pending.Len() > 0 {
		// THE ANSWER, ON THE ONLY PATH LEFT. An empty pending posts NOTHING: everything the model wrote had
		// already reached the card before the human stopped it, so there is nothing to repeat.
		if err := o.deps.Slack.PostMessage(closeCtx, o.channel, o.threadTS, stoppedAnswerPrefix+o.pending.String()); err != nil {
			closeErr = fmt.Errorf("relay: post session %s's answer after the human stopped the live stream: %w", sessionID, err)
		} else {
			o.pending.Reset()
			flushed = true
		}
	}

	// THE CURSOR CATCHES UP HERE OR NOWHERE. delivered() refuses to advance past text still sitting in
	// pending, so a run whose last appends failed and whose closing flush then SUCCEEDED would otherwise
	// leave a cursor pointing at the last confirmed append — and the next run in this session would replay
	// everything after it, re-showing an answer the reader already has. The final flush is the moment that
	// text becomes visible, so it is the moment the cursor may move.
	if flushed && o.handled > o.deliveredSeq {
		o.deliveredSeq = o.handled
		_ = o.deps.Delivery.Delivered(closeCtx, o.handled)
	}

	// ONLY A TERMINAL CLEARS THE DURABLE RECORD, and the condition is the run's own outcome rather than
	// this function being reached: stop() runs on EVERY exit, including the ones that mean the opposite of
	// finished. A read error, a cancelled context and a panic all leave the thread marked pending, which is
	// what makes a killed process recoverable at all — the record is the only thing that will still name
	// this thread once this process is gone. A failure to clear is harmless in the other direction: the
	// next boot re-attaches, sees a journal already at its terminal, renders nothing new and clears it then.
	if o.terminal {
		if err := o.deps.Delivery.Closed(closeCtx); err != nil && closeErr == nil {
			closeErr = fmt.Errorf("relay: clear the delivery record for session %s: %w", sessionID, err)
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
		// Progress adds a line UNDER the step that is running, so a long tool reads as one step reporting
		// itself rather than a growing pile of rows.
		//
		// A notification with nothing to say sends NOTHING, and a notification for a call this relay never
		// saw start sends nothing either: without the card, there is no id to hang the line on.
		detail := toolProgressDetail(event.Data)
		card := o.cards.progress(event.Data)
		if detail == "" || card == nil {
			return
		}
		o.updateCard(ctx, card.task(card.addCaption(detail)))
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
		// cheaper to decide.
		if text, ok := modelStepHeadline[event.Type]; ok {
			o.setHeadline(ctx, text)
			return
		}
		if outcome, ok := runTerminals[event.Type]; ok {
			o.runEnded(ctx, outcome)
			return
		}
		if event.Type == "tool_call.executing.v1" {
			o.stepBegan(ctx, event.Data)
			return
		}
		if status, ok := toolCallStatus[event.Type]; ok {
			o.stepEnded(ctx, event.Data, status)
		}
	}
}

// stepBegan draws the step that just started — the one moment a card learns anything, because it is the only
// event in a tool call's life that carries the tool's NAME and its `arguments_summary`.
//
// THE CAPTION IS THE WHOLE POINT OF THIS FUNCTION. Until commit 9172fea9 an executing frame was exactly
// {run_id, tool_name, tool_call_id, replay_class} and the command, the path and the query sat on
// `tool_calls.arguments` with no route onto the stream — so the honest card could only say "Running a
// command", and it said it eighteen times in a row. The frame now carries a bounded, redacted one-line
// summary, and a step that has one says what it is doing.
//
// A STEP WITH NO CAPTION IS NOT A DEGRADED STEP, it is the previous behaviour exactly: the card falls back
// to the tool's human phrase, which is what every card said before. That is the MCP and remote-HTTP case,
// where a summary is deliberately withheld (a third party owns the schema and it may grow an `api_key`
// property tomorrow), and it will stay the case for any tool nobody has written a summariser for.
func (o *openStream) stepBegan(ctx context.Context, data map[string]any) {
	card, detail := o.cards.begin(data)
	if card == nil {
		return
	}
	o.setHeadline(ctx, card.live())
	o.updateCard(ctx, card.task(detail))
}

// stepEnded resolves the call and re-draws the card it belongs to.
//
// IT SENDS NO DETAIL, because a call's terminal event learns nothing a reader has not already been told: the
// caption went up when the step started, and the status pill carries the rest. Sending anything here would
// APPEND to the card's list, so a "finished" line per call would double the length of every group.
func (o *openStream) stepEnded(ctx context.Context, data map[string]any, status string) {
	if card := o.cards.resolve(data, status); card != nil {
		o.updateCard(ctx, card.task(""))
	}
}

// runEnded is everything a run terminal owes a reader, in the order a reader meets it.
//
// EVERY STILL-OPEN CARD IS RESOLVED, and this is the only place that can do it: a run that dies mid-tool —
// killed, timed out, budget exhausted — leaves its step `in_progress`, and Slack stores such a card as
// `error` when chat.stopStream lands (measured 2026-08-04). Saying it explicitly means the card reports the
// run's own outcome rather than an inference, and a run that ended cleanly cannot leave a spinning row
// behind at all.
//
// THE NOTICE IS THE LINE THE OPERATOR'S FAILED RUN NEVER GOT. That run made thirty-three tool calls over six
// and a half minutes, failed, and left a message whose body was EMPTY — a red card and not one word telling
// a human what had happened. A terminal that is not `completed` has no words of its own by definition, so
// this is the only place they can come from. `completed` writes nothing: the model's answer speaks for it.
func (o *openStream) runEnded(ctx context.Context, outcome runOutcome) {
	// Recorded FIRST, before any of the calls below can fail: what this flag means is "the run is over",
	// which is true the instant the terminal event was read, and it must not become conditional on Slack
	// taking the decorations that report it. A thread left marked pending because a headline update 429'd
	// would be re-attached on every boot for a run that finished long ago.
	o.terminal = true
	for _, card := range o.cards.all {
		if card.status() == "in_progress" {
			card.inFlight = 0
			card.outcome = outcome.card
			o.updateCard(ctx, card.task(""))
		}
	}
	o.setHeadline(ctx, outcome.headline(o.cards.steps, time.Since(o.startedAt)))
	o.emitStatus(ctx, outcome.notice)
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
	"str_replace_based_edit_tool":      "Editing a file",
	"palai.workspace.glob":             "Finding files",
	"palai.workspace.grep":             "Searching the code",
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
