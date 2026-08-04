package relay

import (
	"context"
	"errors"
	"fmt"

	"strings"
	"sync"
	"testing"

	slack "github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// recordedDelivery is Delivery's test double. It records the three calls IN ORDER, because order is the
// whole contract: an Opened after the first Delivered would mean a stream nobody could have resumed, and a
// Closed on a run that never terminated would mean an answer silently written off.
type recordedDelivery struct {
	mu        sync.Mutex
	openedTS  []string
	delivered []int64
	closed    int
	failClose error
}

func (d *recordedDelivery) Opened(_ context.Context, streamTS string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.openedTS = append(d.openedTS, streamTS)
	return nil
}

func (d *recordedDelivery) Delivered(_ context.Context, sequence int64) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.delivered = append(d.delivered, sequence)
	return nil
}

func (d *recordedDelivery) Closed(context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed++
	return d.failClose
}

func (d *recordedDelivery) last() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.delivered) == 0 {
		return 0
	}
	return d.delivered[len(d.delivered)-1]
}

// seq stamps sequence numbers onto a fixture's events, 1..n, so a test can talk about "the cursor after
// the third event" without hand-numbering every literal.
func seq(events []palai.Event) []palai.Event {
	for i := range events {
		events[i].Sequence = i + 1
	}
	return events
}

// -------------------------------------------------------------------------------------------------
// the cursor
// -------------------------------------------------------------------------------------------------

// A RUN THAT FINISHES RECORDS EVERY EVENT AND THEN CLEARS ITSELF. This is the ordinary path, and it is
// asserted first because everything else is a deviation from it.
func TestAFinishedRunRecordsItsProgressAndClearsThePendingMark(t *testing.T) {
	events := seq([]palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "hello"}},
		{Type: "run.completed.v1"},
	})
	fake := &fakeSlack{}
	rec := &recordedDelivery{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals, Delivery: rec},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// "1.1" is what this double's chat.startStream answers; the point is that the recorded value is the
	// ts SLACK gave back, not one this package invented.
	if len(rec.openedTS) != 1 || rec.openedTS[0] != "1.1" {
		t.Fatalf("Opened recorded %v, want exactly the ts chat.startStream returned — a stream nobody wrote down cannot be resumed", rec.openedTS)
	}
	if rec.last() != 2 {
		t.Fatalf("the cursor ended at %d, want 2 (the terminal's own sequence): %v", rec.last(), rec.delivered)
	}
	if rec.closed != 1 {
		t.Fatalf("Closed fired %d time(s), want exactly 1 — a finished run that stays marked pending is re-attached on every boot", rec.closed)
	}
}

// THE CURSOR MUST NOT OVERTAKE SLACK. This is the property that makes the cursor safe to use as a
// de-duplication rule: it is also the resume point, so a number recorded for text that never reached the
// message would make a recovery SKIP that text forever.
func TestTheCursorDoesNotAdvancePastTextSlackRefused(t *testing.T) {
	events := seq([]palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "first "}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "second "}},
	})
	// Every append fails, and the close fails too, so nothing this run wrote ever became visible.
	fake := &fakeSlack{appendErr: errors.New("slack is down"), failStop: true}
	rec := &recordedDelivery{}
	err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals, Delivery: rec},
		"sess_1", "C1", "1.1")
	if err == nil {
		t.Fatal("Run succeeded although no text reached Slack")
	}
	if len(rec.delivered) != 0 {
		t.Fatalf("the cursor advanced to %v although every append and the close failed — a recovery would skip text nobody has seen", rec.delivered)
	}
	if rec.closed != 0 {
		t.Fatalf("Closed fired %d time(s) for a run with no terminal event — the thread would stop being recoverable", rec.closed)
	}
}

// AND IT CATCHES UP WHEN THE CLOSING FLUSH LANDS. The mirror of the test above: text held in pending
// becomes visible at chat.stopStream, so that is the moment the cursor may move past it. Without this the
// NEXT run in the same session would resume from before it and re-show the whole answer.
func TestTheCursorCatchesUpWhenTheClosingFlushSucceeds(t *testing.T) {
	events := seq([]palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "first "}},
		{Type: "run.completed.v1"},
	})
	fake := &fakeSlack{appendErr: errors.New("transient")} // appends fail, the close does not
	rec := &recordedDelivery{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals, Delivery: rec},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.last() != 2 {
		t.Fatalf("the cursor ended at %d, want 2 — the closing flush delivered what the appends could not: %v", rec.last(), rec.delivered)
	}
}

// A RUN THAT STOPS WITHOUT A TERMINAL STAYS PENDING. This is what makes a killed process recoverable at
// all: stop() runs on every exit, and only the run's own terminal is allowed to say "nobody owes this
// thread anything".
func TestAStreamThatEndsWithoutATerminalStaysPending(t *testing.T) {
	events := seq([]palai.Event{{Type: "model_step.delta.v1", Data: map[string]any{"text": "half an answer"}}})
	rec := &recordedDelivery{}
	// staticStream ends in io.EOF, which Run treats as a clean exit — the exact shape a dropped connection
	// takes, and the one a "we reached stop(), so we are done" design would silently mark finished.
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: &fakeSlack{}, OnApproval: noApprovals, Delivery: rec},
		"sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if rec.closed != 0 {
		t.Fatalf("Closed fired %d time(s) for a stream that ended before the run did — that answer becomes unrecoverable", rec.closed)
	}
	if rec.last() != 1 {
		t.Fatalf("the cursor ended at %d, want 1 — the delivered half must still be remembered so the recovery does not repeat it", rec.last())
	}
}

// -------------------------------------------------------------------------------------------------
// resuming
// -------------------------------------------------------------------------------------------------

// THE RECOVERED HALF LANDS IN THE MESSAGE THE DEAD PROCESS OPENED. Measured live to be possible across
// processes (see relay.go's ResumeTS); this asserts the relay actually does it rather than opening a
// second message under the first.
func TestAResumedRunFinishesTheMessageTheDeadProcessOpened(t *testing.T) {
	events := seq([]palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "the rest of the answer"}},
		{Type: "run.completed.v1"},
	})
	fake := &fakeSlack{}
	rec := &recordedDelivery{}
	if err := Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals,
		Delivery: rec, ResumeTS: "1785000000.000100"}, "sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if fake.started != 0 {
		t.Fatalf("chat.startStream was called %d time(s) on a resume — the reader would get a second message under the one they are already watching", fake.started)
	}
	if fake.stoppedTS != "1785000000.000100" {
		t.Fatalf("the close addressed %q, want the resumed ts — a stream closed at the wrong ts leaves the real one streaming forever", fake.stoppedTS)
	}
	if got := strings.Join(fake.appended, ""); !strings.Contains(got, "the rest of the answer") {
		t.Fatalf("the resumed stream carried %q, want the remaining answer", got)
	}
	if len(rec.openedTS) != 0 {
		t.Fatalf("Opened fired on a resume (%v) — the ts was already recorded by the process that opened it", rec.openedTS)
	}
}

// A RESUME DOES NOT REWRITE THE HEADLINE. The message being joined has been showing whatever the dead
// process last reported; replacing that with "Working…" would erase the most recent thing a reader was
// told and make a nearly-finished run look like one that just started.
func TestAResumedRunDoesNotReopenWithTheWorkingHeadline(t *testing.T) {
	fake := &fakeSlack{}
	rec := &recordedDelivery{}
	if err := Run(context.Background(), Deps{Events: staticStream(seq([]palai.Event{{Type: "run.completed.v1"}})),
		Slack: fake, OnApproval: noApprovals, Delivery: rec, ResumeTS: "1785000000.000100"}, "sess_1", "C1", "1.1"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, headline := range fake.plans {
		if headline == openingHeadline {
			t.Fatalf("a resumed run wrote %q over the container headline; the plans were %v", openingHeadline, fake.plans)
		}
	}
}

// -------------------------------------------------------------------------------------------------
// the boot scan
// -------------------------------------------------------------------------------------------------

// resumeFixture builds an InboundDeps whose store already holds one thread left mid-answer, the way a
// killed process leaves it.
type resumeFixture struct {
	deps   InboundDeps
	store  *fakeStore
	palai  *fakePalai
	slacks map[string]*fakeSlack
	logs   []string
	mu     sync.Mutex
}

func newResumeFixture(t *testing.T, streamTS string, lastSeq int64, events []palai.Event) *resumeFixture {
	t.Helper()
	f := &resumeFixture{store: newFakeStore(), slacks: map[string]*fakeSlack{}}
	f.palai = &fakePalai{stream: func(context.Context) EventStream { return staticStream(events) }}

	key := threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"}
	if _, err := f.store.BindThread(context.Background(), key.botID, key.teamID, key.channelID, key.threadTS, "sess_1"); err != nil {
		t.Fatalf("seed BindThread: %v", err)
	}
	if _, err := f.store.BeginDelivery(context.Background(), key.botID, key.teamID, key.channelID, key.threadTS, "U_HUMAN"); err != nil {
		t.Fatalf("seed BeginDelivery: %v", err)
	}
	if streamTS != "" {
		if err := f.store.RecordStreamTS(context.Background(), key.botID, key.teamID, key.channelID, key.threadTS, streamTS); err != nil {
			t.Fatalf("seed RecordStreamTS: %v", err)
		}
	}
	if lastSeq > 0 {
		if err := f.store.RecordDelivered(context.Background(), key.botID, key.teamID, key.channelID, key.threadTS, lastSeq); err != nil {
			t.Fatalf("seed RecordDelivered: %v", err)
		}
	}

	f.deps = NewInboundDeps(f.store, f.palai,
		func(recipientUserID, recipientTeamID string) Slack {
			f.mu.Lock()
			defer f.mu.Unlock()
			s := &fakeSlack{}
			f.slacks[recipientUserID+"/"+recipientTeamID] = s
			return s
		},
		func(fn func()) { fn() }, // synchronous: every assertion after the scan already reflects it
		noApprovals,
		func(error, string, string, string) {},
		key.botID, "U_BOT", "arev_1", "")
	f.deps.Logf = func(format string, args ...any) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.logs = append(f.logs, fmt.Sprintf(format, args...))
	}
	return f
}

func (f *resumeFixture) log() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.logs, "\n")
}

// THE WHOLE POINT, in one test: a thread left mid-answer gets its answer, from the journal, into the same
// message, with no new message and no second run.
func TestTheBootScanFinishesAnAnswerTheDeadProcessNeverDelivered(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Sequence: 7, Data: map[string]any{"text": "the answer nobody saw"}},
		{Type: "run.completed.v1", Sequence: 8},
	}
	f := newResumeFixture(t, "1785000000.000100", 6, events)

	if err := RecoverPendingRuns(context.Background(), f.deps); err != nil {
		t.Fatalf("RecoverPendingRuns: %v", err)
	}

	// It resumed from the recorded cursor, not from zero. A recovery that opened the journal at 0 would
	// re-render the whole run — and, worse, would stop at the FIRST terminal it met.
	if len(f.palai.sessionEventsParams) != 1 || f.palai.sessionEventsParams[0].AfterSequence != 6 {
		t.Fatalf("the journal was opened with %v, want exactly one read after_sequence=6", f.palai.sessionEventsParams)
	}
	// No run was created. Recovery is a read of what already happened.
	if f.palai.responses != 0 || f.palai.sessionsCreated != 0 {
		t.Fatalf("recovery created %d response(s) and %d session(s), want 0 of each — it must never re-run anything", f.palai.responses, f.palai.sessionsCreated)
	}
	sl := f.slacks["U_HUMAN/T1"]
	if sl == nil {
		t.Fatalf("no Slack client was built for the recorded recipient; built: %v", f.slacks)
	}
	if sl.started != 0 {
		t.Fatalf("chat.startStream was called %d time(s); the message was already open and must be finished, not duplicated", sl.started)
	}
	if got := strings.Join(sl.appended, ""); !strings.Contains(got, "the answer nobody saw") {
		t.Fatalf("the recovered stream carried %q, want the undelivered answer", got)
	}
	// And the thread is no longer pending, so the next boot does nothing.
	pending, err := f.store.PendingDeliveries(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("PendingDeliveries: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("%d thread(s) still marked pending after a completed recovery: %v", len(pending), pending)
	}
}

// THE OTHER RECOVERY: the process died between an accepted run and chat.startStream, so there is no
// message to finish and one has to be opened. Without the recorded recipient this branch cannot exist at
// all — slack.StartStream refuses an empty one before it ever calls Slack.
func TestTheBootScanOpensAMessageForARunThatNeverGotOne(t *testing.T) {
	events := []palai.Event{
		{Type: "model_step.delta.v1", Sequence: 1, Data: map[string]any{"text": "an answer with no message"}},
		{Type: "run.completed.v1", Sequence: 2},
	}
	f := newResumeFixture(t, "", 0, events)

	if err := RecoverPendingRuns(context.Background(), f.deps); err != nil {
		t.Fatalf("RecoverPendingRuns: %v", err)
	}
	sl := f.slacks["U_HUMAN/T1"]
	if sl == nil || sl.started != 1 {
		t.Fatalf("chat.startStream was called %v time(s), want exactly 1 — there was no message to resume", sl)
	}
	if got := strings.Join(sl.appended, ""); !strings.Contains(got, "an answer with no message") {
		t.Fatalf("the opened stream carried %q, want the answer", got)
	}
}

// A VANISHED SESSION IS NOT RETRIED FOREVER, and the card it left behind is closed rather than abandoned.
// A 404 here is Task 8's expected steady state reached while the bot was down; a recovery that kept the
// row would 404 on every boot from then on, and the thread would keep a live card over an answer that can
// never arrive.
func TestTheBootScanClosesTheStreamOfASessionThatNoLongerExists(t *testing.T) {
	f := newResumeFixture(t, "1785000000.000100", 3, nil)
	f.palai.stream = func(context.Context) EventStream { return nil }
	f.palai.failNextEventsNotFound = true

	if err := RecoverPendingRuns(context.Background(), f.deps); err != nil {
		t.Fatalf("RecoverPendingRuns: %v", err)
	}
	sl := f.slacks["U_HUMAN/T1"]
	if sl == nil || sl.stopped != 1 {
		t.Fatalf("the abandoned stream was closed %v time(s), want 1 — otherwise it renders as streaming forever", sl)
	}
	pending, err := f.store.PendingDeliveries(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("PendingDeliveries: %v", err)
	}
	if len(pending) != 0 {
		t.Fatalf("the vanished session is still marked pending (%v); every future boot would retry a read that can only 404", pending)
	}
}

// A TRANSIENT FAILURE KEEPS THE DEBT. The distinction from the test above is the whole reason isNotFound is
// consulted at all: a control plane that is briefly unreachable must not be read as "this answer is gone".
func TestATransientReattachFailureLeavesTheThreadPending(t *testing.T) {
	f := newResumeFixture(t, "1785000000.000100", 3, nil)
	f.palai.stream = func(context.Context) EventStream { return nil }
	f.palai.failNextEvents = errors.New("connection refused")

	if err := RecoverPendingRuns(context.Background(), f.deps); err != nil {
		t.Fatalf("RecoverPendingRuns: %v", err)
	}
	pending, err := f.store.PendingDeliveries(context.Background(), "bot_1")
	if err != nil {
		t.Fatalf("PendingDeliveries: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d thread(s) pending after a transient failure, want 1 — the answer is still owed", len(pending))
	}
	if !strings.Contains(f.log(), "the next start will try again") {
		t.Fatalf("the log did not say the debt survives; it said:\n%s", f.log())
	}
}

// A CLEAN BOOT SAYS AND DOES NOTHING. The scan runs on every start, so the empty case has to be free of
// both work and noise.
func TestTheBootScanIsSilentWhenNothingWasInterrupted(t *testing.T) {
	f := newResumeFixture(t, "", 0, nil)
	if err := f.store.EndDelivery(context.Background(), "bot_1", "T1", "C1", "1.1"); err != nil {
		t.Fatalf("EndDelivery: %v", err)
	}
	if err := RecoverPendingRuns(context.Background(), f.deps); err != nil {
		t.Fatalf("RecoverPendingRuns: %v", err)
	}
	if len(f.palai.sessionEventsParams) != 0 {
		t.Fatalf("a clean boot opened %d journal stream(s), want 0", len(f.palai.sessionEventsParams))
	}
	if f.log() != "" {
		t.Fatalf("a clean boot logged %q, want nothing", f.log())
	}
}

// -------------------------------------------------------------------------------------------------
// the cursor survives a restart, which is the older defect this work also closes
// -------------------------------------------------------------------------------------------------

// A FRESH PROCESS OPENS A THREAD'S NEXT RUN AFTER THE PREVIOUS RUN'S TERMINAL, not at zero.
//
// This is not the same defect as a lost answer and it is older: the resume cursor lived only in
// inboundState, so after every restart the first message in an existing thread opened its stream at
// sequence 0 — where the FIRST event Run meets is the previous run's terminal, on which it closes
// immediately. The reader would see the last answer's ending in place of this turn's.
func TestANewProcessResumesAThreadsJournalAfterThePreviousRun(t *testing.T) {
	f := newResumeFixture(t, "", 11, nil)
	if err := f.store.EndDelivery(context.Background(), "bot_1", "T1", "C1", "1.1"); err != nil {
		t.Fatalf("EndDelivery: %v", err)
	}
	// A BRAND NEW InboundDeps over the SAME store: exactly what a restarted process has — an empty
	// inboundState and a database that remembers.
	fresh := NewInboundDeps(f.store, f.palai,
		func(string, string) Slack { return &fakeSlack{} },
		func(fn func()) { fn() }, noApprovals, func(error, string, string, string) {},
		"bot_1", "U_BOT", "arev_1", "")

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage, TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", MessageTS: "1.2", UserID: "U_HUMAN", Text: "another question"}
	if err := HandleEvent(context.Background(), fresh, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	if len(f.palai.sessionEventsParams) != 1 {
		t.Fatalf("the turn opened %d journal stream(s), want 1", len(f.palai.sessionEventsParams))
	}
	if got := f.palai.sessionEventsParams[0].AfterSequence; got != 11 {
		t.Fatalf("a restarted process opened the journal at after_sequence=%d, want 11 — at 0 it reads the PREVIOUS run's terminal first and renders that answer's ending instead of this one's", got)
	}
}

// -------------------------------------------------------------------------------------------------
// the wiring the production path cannot opt out of
// -------------------------------------------------------------------------------------------------

// EVERY RUN THIS FILE STARTS CARRIES A DURABLE RECORD, and it is asserted through the ordinary inbound
// path rather than by reading the code, because "startRun builds one" is exactly the kind of claim that
// stays true in a comment after it stops being true in the function.
func TestEveryRunStartedByAnInboundEventRecordsItsDelivery(t *testing.T) {
	f := newResumeFixture(t, "", 0, []palai.Event{{Type: "run.completed.v1", Sequence: 4}})
	if err := f.store.EndDelivery(context.Background(), "bot_1", "T1", "C1", "1.1"); err != nil {
		t.Fatalf("EndDelivery: %v", err)
	}
	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage, TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", MessageTS: "1.9", UserID: "U_HUMAN", Text: "hello"}
	if err := HandleEvent(context.Background(), f.deps, ev); err != nil {
		t.Fatalf("HandleEvent: %v", err)
	}
	// The run reached its terminal, so the record is cleared — and the cursor it left behind is the
	// terminal's own sequence, which is what the NEXT turn in this thread will resume after.
	row := f.store.delivery[threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"}]
	if row == nil {
		t.Fatal("the turn wrote no delivery row at all — nothing would have named this thread if the process had died")
	}
	if row.runPending {
		t.Fatal("the thread is still marked pending after a completed run")
	}
	if row.lastSequence != 4 {
		t.Fatalf("the cursor is %d, want 4 (the terminal's sequence)", row.lastSequence)
	}
	if row.recipientUserID != "U_HUMAN" {
		t.Fatalf("the recorded recipient is %q, want U_HUMAN — a recovery that has to OPEN a message cannot invent one", row.recipientUserID)
	}
}

// A RUN IS NOT STARTED AT ALL IF ITS PENDING MARK CANNOT BE WRITTEN. The asymmetry with the search grant
// (which fails soft, costing a tool) is deliberate: this failure costs the entire answer if the process
// does not survive the run, so the human is told now rather than promised something only a healthy
// process can deliver.
func TestATurnIsRefusedWhenItsPendingMarkCannotBeRecorded(t *testing.T) {
	f := newResumeFixture(t, "", 0, nil)
	if err := f.store.EndDelivery(context.Background(), "bot_1", "T1", "C1", "1.1"); err != nil {
		t.Fatalf("EndDelivery: %v", err)
	}
	// An unbound thread is the store's own refusal (BeginDelivery has no row to update).
	delete(f.store.bound, threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"})
	delete(f.store.delivery, threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"})
	f.store.bound[threadKey{botID: "bot_1", teamID: "T1", channelID: "C1", threadTS: "1.1"}] = "sess_1"
	f.store.failBeginDelivery = errors.New("the database is unreachable")

	ev := slack.Event{Type: "app_mention", Kind: slack.KindMessage, TeamID: "T1", ChannelID: "C1",
		ThreadTS: "1.1", MessageTS: "1.7", UserID: "U_HUMAN", Text: "hello"}
	err := HandleEvent(context.Background(), f.deps, ev)
	if err == nil {
		t.Fatal("the turn was accepted although nothing could record that an answer was owed")
	}
	if !strings.Contains(err.Error(), "record the pending run") {
		t.Fatalf("the refusal reads %q, want it to name what could not be recorded", err)
	}
}
