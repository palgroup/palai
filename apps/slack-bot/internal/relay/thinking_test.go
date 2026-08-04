package relay

import (
	"slices"
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// THE FIXTURE IS A REAL RUN'S JOURNAL, not an invention. Every event below is what this deployment
// actually wrote for session events 4-17 on 2026-08-05, read straight out of the events table:
//
//	seq  4  model_step.created.v1     mreq_6640105a177193b554eea33b
//	seq  5  model_step.thinking.v1    "The user is asking in Turkish to find prime numbers…"   (232 bytes)
//	seq  6  model_step.thinking.v1    "\n\nI'll go with the inclusive interpretation…for"      (101 bytes)
//	seq  7  model_step.delta.v1       "## 17 ile 23 Arasındaki Asal Sayılar\n\nBu aralıktaki…"
//	seq  8  model_step.thinking.v1    " such problems. The primes are 17, 19, and 23…"         (172 bytes)
//	seq 16  model_step.completed.v1   mreq_6640105a177193b554eea33b
//
// TWO PROPERTIES OF THAT SEQUENCE ARE WHY IT IS COPIED RATHER THAN SIMPLIFIED, and a tidier fixture
// would have tested neither. The reasoning INTERLEAVES with the answer — seq 7 sits between two thinking
// windows, because the two coalescing sinks flush on their own independent tickers — so "reasoning
// first, then the answer" is not a shape this relay may assume. And windows 2 and 3 split a sentence
// between them ("…the more common approach for" / " such problems."), which is what makes the
// no-separator append the only correct one.
const (
	liveStepID   = "mreq_6640105a177193b554eea33b"
	liveThink1   = "The user is asking in Turkish to find prime numbers between 17 and 23 and explain why they're prime. I need to clarify whether the range is inclusive or exclusive, then identify which numbers in that range are prime and verify them."
	liveThink2   = "\n\nI'll go with the inclusive interpretation (17 through 23) since that's the more common approach for"
	liveThink3   = " such problems. The primes are 17, 19, and 23—each divisible only by 1 and itself—while 18, 20, 21, and 22 all have additional divisors. I'll present the answer in Turkish."
	liveAnswer1  = "## 17 ile 23 Arasındaki Asal Sayılar\n\nBu aralıktaki sayılar: **17, 18, 19, 20, 21, 22, 23**"
	liveAnswer2  = "\n\n**17**, **19** ve **23** asaldır."
	liveReasoned = liveThink1 + liveThink2 + liveThink3
)

func thinkingEvent(stepID, text string) palai.Event {
	return palai.Event{Type: modelStepThinkingEventType,
		Data: map[string]any{"model_request_id": stepID, "run_id": "run_1", "thinking": text}}
}

func stepCreated(stepID string) palai.Event {
	return palai.Event{Type: "model_step.created.v1", Data: map[string]any{"model_request_id": stepID}}
}

func stepCompleted(stepID string) palai.Event {
	return palai.Event{Type: "model_step.completed.v1", Data: map[string]any{"model_request_id": stepID}}
}

func answerDelta(text string) palai.Event {
	return palai.Event{Type: "model_step.delta.v1", Data: map[string]any{"text": text}}
}

// liveRun is the measured journal above, replayed.
func liveRun() []palai.Event {
	return []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, liveThink1),
		thinkingEvent(liveStepID, liveThink2),
		answerDelta(liveAnswer1),
		thinkingEvent(liveStepID, liveThink3),
		answerDelta(liveAnswer2),
		stepCompleted(liveStepID),
		{Type: "run.completed.v1", Data: map[string]any{}},
	}
}

// thinkingCards returns the task updates that belong to reasoning cards.
func thinkingCards(tasks []slack.Task) []slack.Task {
	var out []slack.Task
	for _, task := range tasks {
		if strings.HasPrefix(task.Title, thinkingCardMark) {
			out = append(out, task)
		}
	}
	return out
}

// TestReasoningNeverReachesTheAnswerBody is THE separation test, and it reports the leak by name.
//
// WHY IT IS WORTH A TEST OF ITS OWN. `model_step.thinking.v1` exists as a separate event type for
// exactly one structural reason (packages/coordinator/mcp_progress.go): the journal fans out to webhook
// endpoints that subscribe by TYPE, so reasoning riding as a field on the answer's event would have been
// undeliverable-without. This relay is where that separation is either kept or thrown away — every path
// out of handle() leads either to o.emit (the message BODY, which is the answer, permanently, for as
// long as the message exists) or to a card. One misrouted case here would put the model's private
// working into the answer a human reads and quotes, and it would do it in the MIDDLE of that answer,
// because the fixture proves the two streams interleave.
//
// The assertion is over every surface that writes prose — the opening text, every append, the closing
// flush and the rescue message — rather than over "the body" as one string, so a leak cannot hide in the
// one path a narrower test forgot.
func TestReasoningNeverReachesTheAnswerBody(t *testing.T) {
	fake := runCards(t, liveRun())

	body := map[string][]string{
		"chat.startStream markdown_text": {fake.startedText},
		"chat.appendStream markdown_text": fake.appended,
		"chat.stopStream markdown_text":  {fake.stoppedText},
		"chat.postMessage (the rescue path)": fake.posted,
	}
	// Every fragment separately: a check for the whole reasoning would pass on a relay that leaked half
	// of it, and half of a model's private working is still the model's private working.
	for _, fragment := range []string{liveThink1, liveThink2, liveThink3} {
		for surface, texts := range body {
			for _, text := range texts {
				if strings.Contains(text, strings.TrimSpace(fragment)) {
					t.Fatalf("THE MODEL'S REASONING LEAKED INTO THE ANSWER: %s carries %q.\n"+
						"That text is model_step.thinking.v1's `thinking` field — the model's private working — and the "+
						"body of this message is the answer a human reads, quotes and keeps. It is a separate event type "+
						"precisely so that no reader takes one for the other.",
						surface, strings.TrimSpace(fragment)[:60]+"…")
				}
			}
		}
	}

	// AND THE OTHER DIRECTION, without which the test above passes on a relay that renders nothing at
	// all. The reasoning has to be SOMEWHERE, and that somewhere is the card.
	var drawn strings.Builder
	for _, task := range thinkingCards(fake.tasks) {
		drawn.WriteString(task.Detail)
	}
	if got := drawn.String(); got != liveReasoned {
		t.Fatalf("the reasoning cards hold %q, want the run's whole reasoning %q", got, liveReasoned)
	}
}

// TestTheAnswerStillReachesTheBody is the guard on the guard: a relay that dropped model_step.delta.v1
// would pass the leak test perfectly.
func TestTheAnswerStillReachesTheBody(t *testing.T) {
	fake := runCards(t, liveRun())
	body := fake.startedText + strings.Join(fake.appended, "") + fake.stoppedText
	if !strings.Contains(body, liveAnswer1) || !strings.Contains(body, liveAnswer2) {
		t.Fatalf("the message body is %q, want both of the model's answer deltas in it", body)
	}
}

// TestReasoningWindowsRejoinIntoOneContinuousText is the measurement that decides addThinking's shape.
//
// The three live windows split a sentence twice. A card that appended them the way it appends tool
// captions — one per line — would render
//
//	…since that's the more common approach for
//	such problems. The primes are 17, 19, and 23…
//
// which is not what the model wrote. `details` appends with no separator of its own, so appending the
// window RAW is what puts the sentence back together, and this asserts the reassembled text byte for
// byte rather than asserting each window is present somewhere.
func TestReasoningWindowsRejoinIntoOneContinuousText(t *testing.T) {
	fake := runCards(t, liveRun())
	cards := thinkingCards(fake.tasks)
	if len(cards) == 0 {
		t.Fatal("no reasoning card was drawn at all")
	}
	var joined strings.Builder
	for _, task := range cards {
		joined.WriteString(task.Detail)
	}
	if got := joined.String(); got != liveReasoned {
		t.Fatalf("the card reassembles the reasoning as\n  %q\nwant\n  %q", got, liveReasoned)
	}
	if strings.Contains(joined.String(), "approach for\n such problems") {
		t.Fatal("a separator was inserted between two windows that continue one sentence")
	}
}

// TestOneCardPerModelStepCarriesThatStepsReasoning: the cards are keyed by model_request_id, so two
// steps get two cards and a step's own windows all land on one.
func TestOneCardPerModelStepCarriesThatStepsReasoning(t *testing.T) {
	const second = "mreq_second"
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "First I need to count the files."),
		thinkingEvent(liveStepID, " Then find the main screen."),
		stepCompleted(liveStepID),
		stepCreated(second),
		thinkingEvent(second, "Now I can answer."),
		stepCompleted(second),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})

	ids := map[string]string{}
	for _, task := range thinkingCards(fake.tasks) {
		ids[task.ID] += task.Detail
	}
	if len(ids) != 2 {
		t.Fatalf("reasoning was drawn on %d card(s) %v, want one per model step", len(ids), ids)
	}
	if got := ids[liveStepID]; got != "First I need to count the files. Then find the main screen." {
		t.Fatalf("the first step's card holds %q", got)
	}
	if got := ids[second]; got != "Now I can answer." {
		t.Fatalf("the second step's card holds %q", got)
	}
}

// TestReasoningDoesNotBreakAToolGroup is decision 2 of the block comment in cards.go, asserted.
//
// A run alternates step/tool/step/tool throughout, so if a reasoning card advanced the grouping pointer
// every burst of consecutive calls would split back into a row per call — undoing the change that took
// the operator's thirty-three rows down to ten, in exactly the runs that need it most.
func TestReasoningDoesNotBreakAToolGroup(t *testing.T) {
	fake := runCards(t, []palai.Event{
		shellStep("tc_1", "swift build"),
		doneStep("tc_1", "palai.workspace.shell"),
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "The build succeeded, so now I run the tests."),
		stepCompleted(liveStepID),
		shellStep("tc_2", "swift test"),
		doneStep("tc_2", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})

	shellCards := map[string]bool{}
	for _, task := range fake.tasks {
		if !strings.HasPrefix(task.Title, thinkingCardMark) {
			shellCards[task.ID] = true
		}
	}
	if len(shellCards) != 1 {
		t.Fatalf("the two shell calls drew %d cards %v, want ONE — a reasoning card between them must not "+
			"start a new group", len(shellCards), shellCards)
	}
}

// TestReasoningIsNotCountedAsAStep: "Done · N steps" counts the run's work, not the thinking about it.
func TestReasoningIsNotCountedAsAStep(t *testing.T) {
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "I should look at the project first."),
		stepCompleted(liveStepID),
		shellStep("tc_1", "swift build"),
		doneStep("tc_1", "palai.workspace.shell"),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	last := fake.plans[len(fake.plans)-1]
	if !strings.Contains(last, "1 step") {
		t.Fatalf("the closing headline is %q, want it to count the ONE tool call and not the reasoning", last)
	}
}

// TestAReasoningCardIsResolvedByItsOwnModelStep — the SLK-P2 rule applied to the new card: a card left
// `in_progress` renders a finished message as one still working.
func TestAReasoningCardIsResolvedByItsOwnModelStep(t *testing.T) {
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "Thinking about it."),
		stepCompleted(liveStepID),
		// Deliberately NO run terminal: this asserts the model step's own terminal did the resolving,
		// not the run's sweep of everything still open.
	})
	cards := thinkingCards(fake.tasks)
	if len(cards) < 2 {
		t.Fatalf("drew %d reasoning updates, want the window and its resolution", len(cards))
	}
	if got := cards[0].Status; got != "in_progress" {
		t.Fatalf("the reasoning card opened %q, want in_progress while the step is still running", got)
	}
	if got := cards[len(cards)-1].Status; got != "done" {
		t.Fatalf("the reasoning card ended %q, want done — a card its own model step never resolved spins "+
			"forever in the finished message", got)
	}
}

// TestEveryModelStepTerminalResolvesTheReasoningCard walks modelStepEnded's OWN table rather than a list
// retyped here, so a terminal added without a rendering rule cannot ship untested.
func TestEveryModelStepTerminalResolvesTheReasoningCard(t *testing.T) {
	for eventType, want := range modelStepEnded {
		t.Run(eventType, func(t *testing.T) {
			fake := runCards(t, []palai.Event{
				stepCreated(liveStepID),
				thinkingEvent(liveStepID, "Working on it."),
				{Type: eventType, Data: map[string]any{"model_request_id": liveStepID}},
			})
			cards := thinkingCards(fake.tasks)
			if got := cards[len(cards)-1].Status; got != want {
				t.Fatalf("%s left the reasoning card %q, want %q", eventType, got, want)
			}
		})
	}
}

// TestAReasoningCardLeftOpenIsResolvedByTheRunTerminal is the floor under the test above: a run that
// dies while the model is still reasoning journals no model step terminal at all.
func TestAReasoningCardLeftOpenIsResolvedByTheRunTerminal(t *testing.T) {
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "Halfway through a thought when the run died."),
		{Type: "run.failed.v1", Data: map[string]any{}},
	})
	cards := thinkingCards(fake.tasks)
	if got := cards[len(cards)-1].Status; got != "failed" {
		t.Fatalf("the reasoning card ended %q, want failed — the run died mid-thought and a card still "+
			"drawn in_progress when chat.stopStream lands reports a finished message as still working", got)
	}
}

// TestTheHeadlineSaysWhatTheModelIsThinkingAbout: the container's one always-visible line stops saying
// the single word "Thinking…" and says what the model is actually working on.
func TestTheHeadlineSaysWhatTheModelIsThinkingAbout(t *testing.T) {
	fake := runCards(t, liveRun())
	var reasoningHeadlines []string
	for _, plan := range fake.plans {
		if strings.HasPrefix(plan, thinkingCardMark) {
			reasoningHeadlines = append(reasoningHeadlines, plan)
		}
	}
	if len(reasoningHeadlines) == 0 {
		t.Fatalf("the headline sequence was %v, want the model's reasoning to reach it", fake.plans)
	}
	if !strings.Contains(reasoningHeadlines[0], "The user is asking in Turkish to find prime numbers") {
		t.Fatalf("the reasoning headline was %q, want the model's opening line", reasoningHeadlines[0])
	}
	if slices.Index(fake.plans, thinkingHeadline) >= 0 &&
		slices.Index(fake.plans, thinkingHeadline) > slices.Index(fake.plans, reasoningHeadlines[0]) {
		t.Fatalf("the headline went BACK to the bare %q after the model had said what it was thinking: %v",
			thinkingHeadline, fake.plans)
	}
}

// TestTheReasoningHeadlineIsWrittenOncePerStep is the cost half of that decision. The headline rides a
// chat.appendStream chunk against a Tier 4 budget shared with the answer's own text, and a run's windows
// arrive every 500ms — so a headline recomputed from the newest window would spend a call per window for
// the whole length of every step.
func TestTheReasoningHeadlineIsWrittenOncePerStep(t *testing.T) {
	fake := runCards(t, liveRun())
	var n int
	for _, plan := range fake.plans {
		if strings.HasPrefix(plan, thinkingCardMark) {
			n++
		}
	}
	if n > 2 {
		t.Fatalf("the ONE model step of this run wrote %d reasoning headlines %v; the lead line closes at "+
			"its first newline or at a headline's worth of runes, which bounds this at about two", n, fake.plans)
	}
}

// TestALeadThatArrivesInPiecesStillFillsTheHeadline: the sink flushes on a timer, so the first window is
// whatever had streamed in 500ms — three words is as legal as a paragraph.
func TestALeadThatArrivesInPiecesStillFillsTheHeadline(t *testing.T) {
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, "The user"),
		thinkingEvent(liveStepID, " wants the prime numbers"),
		thinkingEvent(liveStepID, " between 17 and 23.\nLet me check each one."),
		stepCompleted(liveStepID),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	last := ""
	for _, plan := range fake.plans {
		if strings.HasPrefix(plan, thinkingCardMark) {
			last = plan
		}
	}
	if want := thinkingCardMark + "The user wants the prime numbers between 17 and 23."; last != want {
		t.Fatalf("the headline settled on %q, want %q — the lead grows until the reasoning's first line "+
			"actually ends", last, want)
	}
}

// TestAReasoningCardsTitleIsOneReadableLine, measured from the live run on 2026-08-05: the lead closes on
// a WINDOW boundary, so it overshoots maxHeadline — two windows of 63 and 65 runes produced a 128-rune
// lead, and the title is the field that labels a row in a list.
//
// The second half is what makes the trim safe rather than lossy, and it is asserted here because a title
// cut with nothing behind it would be a card that shows less than the model thought.
func TestAReasoningCardsTitleIsOneReadableLine(t *testing.T) {
	first := strings.Repeat("checking the range ", 4)  // 76 runes: under the ceiling, so the lead stays open
	second := strings.Repeat("and then verifying ", 4) // pushes it to 152, on a window boundary
	fake := runCards(t, []palai.Event{
		stepCreated(liveStepID),
		thinkingEvent(liveStepID, first),
		thinkingEvent(liveStepID, second),
		stepCompleted(liveStepID),
		{Type: "run.completed.v1", Data: map[string]any{}},
	})
	cards := thinkingCards(fake.tasks)
	last := cards[len(cards)-1]
	if runes := []rune(last.Title); len(runes) > maxHeadline {
		t.Fatalf("the card title runs to %d runes (%q); it labels a row in a list, and the whole lead is in "+
			"the details right below it", len(runes), last.Title)
	}
	if !strings.HasSuffix(last.Title, "…") {
		t.Fatalf("the title %q was cut without saying so", last.Title)
	}
	var details strings.Builder
	for _, card := range cards {
		details.WriteString(card.Detail)
	}
	if got := details.String(); got != first+second {
		t.Fatalf("the details hold %q, want the WHOLE lead the title only labels", got)
	}
}

// TestAWindowWithNothingToIdentifyItDrawsNothing — the same rule begin() applies to a call with no id,
// for the same reason: an empty id is not a card Slack can advance, and two steps sharing one would
// overwrite each other's reasoning.
func TestAWindowWithNothingToIdentifyItDrawsNothing(t *testing.T) {
	for name, event := range map[string]palai.Event{
		"no model_request_id": {Type: modelStepThinkingEventType, Data: map[string]any{"thinking": "a thought"}},
		"no thinking text":    {Type: modelStepThinkingEventType, Data: map[string]any{"model_request_id": liveStepID}},
		"reasoning under the WRONG key": {Type: modelStepThinkingEventType,
			Data: map[string]any{"model_request_id": liveStepID, "text": "reasoning that arrived as `text`"}},
	} {
		t.Run(name, func(t *testing.T) {
			fake := runCards(t, []palai.Event{event, {Type: "run.completed.v1", Data: map[string]any{}}})
			if cards := thinkingCards(fake.tasks); len(cards) != 0 {
				t.Fatalf("drew %+v, want nothing", cards)
			}
		})
	}
}
