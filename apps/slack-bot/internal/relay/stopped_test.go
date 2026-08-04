package relay

import (
	"context"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// THE HUMAN PRESSED STOP ON THE STREAMING CARD (S11). These tests all drive Slack's OWN error code —
// `stopped_by_user`, which is what chat.appendStream answers after the stop button — through the real
// adapter type (*slack.APIError), never a substring or a bespoke sentinel. That matters here more than
// usual: the code is the ONLY thing that distinguishes "the human stopped the rendering" from "Slack is
// unreachable", and the two answers are opposite (deliver the answer another way vs. retry into the same
// stream). A double returning errors.New("stopped_by_user") would pass every assertion below while
// proving nothing about the branch production takes.
//
// WHY NO LIVE LEG: producing this code requires a human finger on the stop button of a live streaming
// card. Nothing this process can call reproduces it — a bot cannot press it, and no Slack API sets a
// stream to stopped. So the evidence is Slack's documented code, driven through the same
// slack.APIErrorCode the production path reads.

// runStopped is the fixture every test here shares: three deltas and a terminal, so there is text on both
// sides of wherever the stop lands.
func runStopped(t *testing.T, fake *fakeSlack, events []palai.Event) error {
	t.Helper()
	return Run(context.Background(), Deps{Events: staticStream(events), Slack: fake, OnApproval: noApprovals, Delivery: &recordedDelivery{}},
		"sess_1", "C1", "1.1")
}

func deltaRun(texts ...string) []palai.Event {
	events := make([]palai.Event, 0, len(texts)+1)
	for _, text := range texts {
		events = append(events, palai.Event{Type: "model_step.delta.v1", Data: map[string]any{"text": text}})
	}
	return append(events, palai.Event{Type: "run.completed.v1", Data: map[string]any{}})
}

// TestAStoppedStreamStillDeliversTheAnswer is THE test for this defect. Before it, every append after the
// stop failed, the closing stop failed, Run returned that error and dispatch.go logged "THE ANSWER NEVER
// REACHED THE THREAD" — the run completed server-side with its text journalled and the person who pressed
// stop got nothing at all.
//
// It asserts the two halves that make the answer whole: the text delivered BEFORE the stop stayed on the
// card, and everything after it arrives as a plain message. Neither alone is the property — a version that
// re-posted the entire answer would duplicate what the reader already has, and one that posted only the
// last delta would silently drop the middle.
func TestAStoppedStreamStillDeliversTheAnswer(t *testing.T) {
	fake := &fakeSlack{stopAfter: 2} // the first append lands; the human presses stop; the rest are refused
	if err := runStopped(t, fake, deltaRun("first. ", "second. ", "third.")); err != nil {
		t.Fatalf("Run: %v — a stopped stream is not a failed run", err)
	}
	if got := strings.Join(fake.appended, ""); got != "first. " {
		t.Fatalf("the card carries %q, want only the text delivered before the stop", got)
	}
	if len(fake.posted) != 2 {
		t.Fatalf("posted %d plain message(s) (%q), want 2 — the notice and the answer", len(fake.posted), fake.posted)
	}
	answer := fake.posted[1]
	if !strings.HasSuffix(answer, "second. third.") {
		t.Fatalf("the posted answer is %q, want it to END with everything the stream refused", answer)
	}
	if strings.Contains(answer, "first.") {
		t.Fatalf("the posted answer is %q, want it NOT to repeat text the card already shows", answer)
	}
}

// TestAStoppedStreamTellsTheThreadTheRunContinues pins the notice, and it is not decoration: the human
// who pressed stop believes they cancelled the run. A thread that then goes quiet reads as "cancelled",
// and the next thing they type STEERS the run they think they killed rather than asking a new question.
//
// It also pins that the notice is posted ONCE. The refusal is permanent, so a version announcing it per
// refused call would post a line per delta for the rest of the run.
func TestAStoppedStreamTellsTheThreadTheRunContinuesAndSaysItOnce(t *testing.T) {
	fake := &fakeSlack{stopAfter: 1} // every append is refused, from the first
	if err := runStopped(t, fake, deltaRun("a", "b", "c", "d")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	notices := 0
	for _, msg := range fake.posted {
		if strings.Contains(msg, "not cancelled") {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("posted %d notices out of %d messages (%q), want exactly 1 — four refused appends must not "+
			"produce four announcements", notices, len(fake.posted), fake.posted)
	}
	if !strings.Contains(fake.posted[0], "not cancelled") {
		t.Fatalf("the FIRST plain message is %q, want the notice — it is only useful before the answer, "+
			"while the human still thinks the run is dead", fake.posted[0])
	}
}

// TestNothingIsWrittenToAStoppedStreamAgain is the cost half. Slack refuses appends on a stopped stream
// FOREVER, so every later call is a round trip that can only fail — spent against the same Tier 4 budget
// the bot's other threads share. A relay that kept trying would make a stopped stream the most expensive
// state a run can be in.
//
// The cards are counted alongside the appends because they travel the same way (a task_update is a chunk
// on this stream), and a long run draws far more of them than it writes deltas.
func TestNothingIsWrittenToAStoppedStreamAgain(t *testing.T) {
	fake := &fakeSlack{stopAfter: 1}
	events := []palai.Event{
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "a"}}, // refused: the stream is stopped
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tcall_1", "tool_name": "palai.workspace.file"}},
		{Type: "tool_call.completed.v1", Data: map[string]any{"tool_call_id": "tcall_1", "tool_name": "palai.workspace.file"}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "b"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	if err := runStopped(t, fake, events); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// NOT ONE CARD REACHES SLACK. The eager write Run makes before reading its first event is the container
	// HEADLINE, not a card, so the stop here is discovered by the first delta and every card after it is
	// skipped — including the run terminal's own sweep, which would otherwise spend an update per open step
	// on a stream that refuses all of them.
	if len(fake.tasks) != 0 {
		t.Fatalf("the relay drew %d cards (%v) on a stopped stream; Slack refuses every one of them",
			len(fake.tasks), fake.tasks)
	}
	// And exactly ONE headline: the opening one, written before the stop was knowable. The run terminal's
	// closing summary must not be attempted on a stream that cannot take it.
	if len(fake.plans) != 1 {
		t.Fatalf("the relay wrote %d headlines (%q), want only the one written before the stop was known",
			len(fake.plans), fake.plans)
	}
	if len(fake.appended) != 0 {
		t.Fatalf("the relay appended %d time(s) to a stopped stream (%q); Slack refuses every one of them", len(fake.appended), fake.appended)
	}
}

// TestACardCanBeWhatDiscoversTheStop is the ordering the delta-driven tests cannot reach: a run whose model
// is composing draws cards between deltas, so the FIRST call Slack refuses can be a task_update rather than
// an append. Learning the stop there is what keeps the rest of the run from spending a wasted round trip per
// card — and, more importantly, what puts the answer on the plain-message path at the end.
//
// THE FIXTURE CARRIES A REAL TOOL CALL and that is not decoration. This test used to run on deltaRun alone,
// which drew no tool card at all: the refusal it measured was the eagerly-opened "Thinking" card, drawn
// before the first event was read. That card is now the container's headline instead, so the same fixture
// would have let the delta through and asserted a card path nothing walked. A test named for a card must
// make a card.
func TestACardCanBeWhatDiscoversTheStop(t *testing.T) {
	fake := &fakeSlack{stopCards: true} // only UpdateTask refuses; AppendStream would still accept text
	events := []palai.Event{
		{Type: "tool_call.executing.v1", Data: map[string]any{"tool_call_id": "tc_1", "tool_name": "palai.workspace.shell"}},
		{Type: "model_step.delta.v1", Data: map[string]any{"text": "the answer"}},
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
	if err := runStopped(t, fake, events); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.tasks) != 0 {
		t.Fatalf("fakeSlack recorded %+v, but every UpdateTask under stopCards refuses — the fixture must be "+
			"reaching a card for this test to mean anything", fake.tasks)
	}
	if len(fake.appended) != 0 {
		t.Fatalf("appended %q after a CARD reported the stream stopped; the refusal covers the whole stream, "+
			"not the call that happened to reveal it", fake.appended)
	}
	if len(fake.posted) != 2 || !strings.Contains(fake.posted[1], "the answer") {
		t.Fatalf("plain messages = %q, want the notice and then the answer", fake.posted)
	}
}

// TestTheHeadlineCanBeWhatDiscoversTheStop is the EARLIEST any run can learn its stream is stopped: Run
// writes the container headline before it has read a single event, so on a stream a human stopped between
// the start and the first chunk, that write is the first refusal there is.
//
// It matters for the same reason the card case does — every later call on a stopped stream is a round trip
// that can only fail — and for one more: this is the only refusal that can happen before the relay has
// anything to deliver, so it is the one that proves the notice does not depend on there being an answer yet.
func TestTheHeadlineCanBeWhatDiscoversTheStop(t *testing.T) {
	fake := &fakeSlack{stopPlans: true} // only UpdatePlan refuses
	if err := runStopped(t, fake, deltaRun("the answer")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.plans) != 0 || len(fake.appended) != 0 {
		t.Fatalf("headlines=%q appends=%q after the opening headline was refused; nothing may be written to a "+
			"stopped stream again", fake.plans, fake.appended)
	}
	if len(fake.posted) != 2 || !strings.Contains(fake.posted[1], "the answer") {
		t.Fatalf("plain messages = %q, want the notice and then the answer — a stop discovered before the run "+
			"produced anything must still deliver what it produces later", fake.posted)
	}
}

// TestAStopDiscoveredAtTheCloseStillDeliversTheAnswer is the case a mid-stream-only fix would miss, and it
// is not a rarity: a human who presses stop while the model is composing the last of its answer produces NO
// further append, so chat.stopStream is the first call Slack refuses — with the whole of the undelivered
// text riding on it, which is precisely where it would be lost.
//
// The notice is deliberately NOT posted here: by the time the close runs the run has ended, so a line
// promising that it is "still working" would be false. The answer's own message explains itself instead.
func TestAStopDiscoveredAtTheCloseStillDeliversTheAnswer(t *testing.T) {
	fake := &fakeSlack{failAppends: 1, stopAfter: 0}
	// failAppends leaves the text in pending; stopOnClose makes only the close refuse.
	fake.stopOnClose = true
	if err := runStopped(t, fake, deltaRun("the whole answer")); err != nil {
		t.Fatalf("Run: %v — text refused by the close must not become a run failure", err)
	}
	if len(fake.posted) != 1 {
		t.Fatalf("posted %q, want exactly the answer: the run has already ended, so a notice saying it is "+
			"still working would be a lie", fake.posted)
	}
	if !strings.Contains(fake.posted[0], "the whole answer") {
		t.Fatalf("the posted message is %q, want the text the closing call could not deliver", fake.posted[0])
	}
}

// TestAStoppedStreamWithNothingLeftPostsNoAnswer is the other side of the same rule: when the stop lands
// after everything has already been delivered, there is nothing to repeat, and a relay that posted the
// whole answer again would show the reader a duplicate of what is already on their screen.
func TestAStoppedStreamWithNothingLeftPostsNoAnswer(t *testing.T) {
	fake := &fakeSlack{stopAfter: 2}
	// One delta, delivered; then the terminal event, whose only Slack call is the close.
	if err := runStopped(t, fake, deltaRun("all of it")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := strings.Join(fake.appended, ""); got != "all of it" {
		t.Fatalf("the card carries %q, want the whole answer — it was delivered before the stop", got)
	}
	if len(fake.posted) != 0 {
		t.Fatalf("posted %q, want nothing: the answer is already on the card and the run never wrote more",
			fake.posted)
	}
}

// TestAFailedPlainPostIsReported keeps the failure honest at the ONE point this relay still has no answer
// for. Everything above turns a refused stream into a delivered answer; if the plain message ALSO fails,
// the text is genuinely lost from Slack's side, and the only remaining response is to say so where an
// operator reads (dispatch.go's onRunFailed). Swallowing it would make an empty thread the quietest
// failure this process has.
func TestAFailedPlainPostIsReported(t *testing.T) {
	fake := &fakeSlack{stopAfter: 1, failPost: true}
	err := runStopped(t, fake, deltaRun("lost text"))
	if err == nil {
		t.Fatal("Run returned nil after the answer's last delivery path failed; the loss must be reported")
	}
	if !strings.Contains(err.Error(), "stopped the live stream") {
		t.Fatalf("Run error = %v, want it to name what failed — an operator reading it has to know the answer "+
			"was refused by chat.postMessage, not by the stream", err)
	}
}

// TestTheStoppedCodeIsTheOnlyOneThatChangesThePath guards the discrimination itself. Every OTHER Slack
// failure keeps the existing behaviour — the text stays in pending, the stream is still closed, the closing
// call is still the last attempt — because for anything but a stopped stream the message IS still writable
// and moving the answer to a second post would split it across two messages for no reason.
func TestTheStoppedCodeIsTheOnlyOneThatChangesThePath(t *testing.T) {
	fake := &fakeSlack{failAppends: 1} // a plain transport failure, no Slack error code at all
	if err := runStopped(t, fake, deltaRun("a", "b")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.posted) != 0 {
		t.Fatalf("posted %q on an ordinary append failure; only stopped_by_user moves the answer off the stream",
			fake.posted)
	}
	if fake.stopped != 1 {
		t.Fatalf("stopped=%d, want 1 — a stream that is merely failing must still be CLOSED, or it renders as "+
			"permanently streaming (SLK-P2)", fake.stopped)
	}
	if got := strings.Join(fake.appended, ""); got != "ab" {
		t.Fatalf("appended %q, want the failed chunk carried by the next call as it always was", got)
	}
}

// TestAnUnrelatedAPIErrorIsNotReadAsAStop is the same discrimination against a NEIGHBOURING code rather
// than against no code at all: *slack.APIError is the type both take, so a check written as `errors.As`
// alone — without comparing the code — would treat `channel_not_found` or a rate limit as the human's stop
// button and post the answer twice.
func TestAnUnrelatedAPIErrorIsNotReadAsAStop(t *testing.T) {
	fake := &fakeSlack{appendErr: &slack.APIError{Code: "channel_not_found"}}
	if err := runStopped(t, fake, deltaRun("a")); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(fake.posted) != 0 {
		t.Fatalf("posted %q for a %q refusal; only stopped_by_user is the human's stop button",
			fake.posted, "channel_not_found")
	}
}
