package relay

import (
	"strings"
	"testing"

	"github.com/palgroup/palai/adapters/integrations/slack"
	palai "github.com/palgroup/palai/sdks/go"
)

// wireSpend is what a card actually cost Slack: the sum, over the updates for ONE card id, of the bytes
// each `details` carried after the two transformations TaskUpdateChunk applies on the way out. It is
// summed because the field APPENDS — the ceiling is on the accumulation, which is the half of the
// measurement a per-chunk check would miss entirely.
func wireSpend(tasks []slack.Task, id string) int {
	var n int
	for _, task := range tasks {
		if task.ID == id {
			n += wireBytes(task.Detail)
		}
	}
	return n
}

// TestTheTruncationNotesFitTheReserve is the guard on the guard.
//
// cardDetailBudget holds back room for whichever note a card might have to end with, and every cut below
// then writes its note unconditionally. If a note grew past that reserve the note itself would push the
// field over the ceiling — and the ceiling's failure mode is that SLACK ANSWERS ok AND STORES NOTHING,
// so the card would lose every line it had written and the truncation would become the silent cut the
// note exists to prevent.
func TestTheTruncationNotesFitTheReserve(t *testing.T) {
	for name, note := range map[string]string{"cardTruncated": cardTruncated, "thinkingTruncated": thinkingTruncated} {
		// +1 for the separator newline a note carries when the details are already non-empty.
		if cost := wireBytes(note) + 1; cardDetailBudget+cost > maxCardDetails {
			t.Fatalf("%s costs %d wire bytes but the reserve is only %d — a card that cuts would push its "+
				"own details past %d, and Slack answers ok and stores NOTHING past that line",
				name, cost, maxCardDetails-cardDetailBudget, maxCardDetails)
		}
	}
}

// TestWireBytesCountsWhatIsSentAndNotWhatIsShown encodes the measurement that decided where the count is
// taken (2026-08-05, live API): 12000 bytes of `\*` displayed as 6000 characters was STORED, and 24000
// bytes of `\*` displayed as 12000 was DROPPED. So the ceiling counts the escaped string, and a budget
// measured on the string this package holds would let a card of ordinary-looking prose blank itself.
func TestWireBytesCountsWhatIsSentAndNotWhatIsShown(t *testing.T) {
	const held = "2*3*4"
	if got, want := wireBytes(held), len(slack.EscapeCardDetails(held)); got != want {
		t.Fatalf("wireBytes(%q) = %d, want the escaped length %d", held, got, want)
	}
	if wireBytes(held) <= len(held) {
		t.Fatalf("wireBytes(%q) = %d did not grow past the %d bytes this package holds; the escape is what "+
			"the ceiling counts and this test would not notice it being dropped", held, wireBytes(held), len(held))
	}
}

// TestAReasoningCardNeverSpendsPastTheCeiling drives the whole relay with a step whose reasoning is far
// larger than the field takes — 40 windows of 1000 characters against a 12,000-byte ceiling.
//
// THE ASSERTION IS ON THE WIRE TOTAL, not on any one update, because the ceiling is on the accumulation:
// a card that sent twenty perfectly legal 1000-byte chunks would be as dead as one that sent 20000 at
// once, and Slack would answer ok to every single call on the way there.
func TestAReasoningCardNeverSpendsPastTheCeiling(t *testing.T) {
	events := []palai.Event{stepCreated(liveStepID)}
	for i := 0; i < 40; i++ {
		events = append(events, thinkingEvent(liveStepID, strings.Repeat("reasoning ", 100)))
	}
	events = append(events, stepCompleted(liveStepID), palai.Event{Type: "run.completed.v1", Data: map[string]any{}})
	fake := runCards(t, events)

	spent := wireSpend(fake.tasks, liveStepID)
	if spent > maxCardDetails {
		t.Fatalf("the reasoning card sent %d wire bytes of details, past the measured %d ceiling — Slack "+
			"accepts that write and stores NOTHING, so the whole card's transcript would be gone",
			spent, maxCardDetails)
	}
	if spent < maxCardDetails/2 {
		t.Fatalf("the reasoning card sent only %d of an available %d bytes; a budget that cuts this early "+
			"is throwing away reasoning that would have fitted", spent, cardDetailBudget)
	}
}

// TestACutReasoningCardSaysSoOutLoud — the coordinator gives its own 16 KiB per-event ceiling a visible
// note for the reason this one needs one: an answer has a second authoritative copy in the model step's
// terminal event and REASONING HAS NONE. A reader who cannot see the cut cannot tell a model that
// stopped reasoning from a card that stopped recording it.
func TestACutReasoningCardSaysSoOutLoud(t *testing.T) {
	events := []palai.Event{stepCreated(liveStepID)}
	for i := 0; i < 40; i++ {
		events = append(events, thinkingEvent(liveStepID, strings.Repeat("reasoning ", 100)))
	}
	events = append(events, palai.Event{Type: "run.completed.v1", Data: map[string]any{}})
	fake := runCards(t, events)

	var joined strings.Builder
	for _, task := range fake.tasks {
		if task.ID == liveStepID {
			joined.WriteString(task.Detail)
		}
	}
	if !strings.Contains(joined.String(), thinkingTruncated) {
		t.Fatalf("the card was cut and said nothing about it; details end with %q",
			joined.String()[max(0, joined.Len()-120):])
	}
	// Written exactly once: a note repeated per window past the ceiling would itself be the overflow.
	if n := strings.Count(joined.String(), thinkingTruncated); n != 1 {
		t.Fatalf("the truncation note appears %d times, want exactly one", n)
	}
}

// TestTheOverflowingWindowKeepsWhatFits. Dropping the window whole would throw away up to a window's
// worth of reasoning that had room — and the window that overflows is by definition the one nearest the
// boundary, so it is usually mostly affordable.
func TestTheOverflowingWindowKeepsWhatFits(t *testing.T) {
	card := &stepCard{thinking: true}
	first := card.addThinking(strings.Repeat("a", cardDetailBudget-100))
	if len(first) != cardDetailBudget-100 {
		t.Fatalf("the first window was already clipped (%d bytes)", len(first))
	}
	second := card.addThinking(strings.Repeat("b", 500))
	kept := strings.TrimSuffix(second, "\n"+thinkingTruncated)
	if kept == second {
		kept = strings.TrimSuffix(second, thinkingTruncated)
	}
	if len(kept) != 100 {
		t.Fatalf("the overflowing window kept %d of the 100 bytes still available: %q", len(kept), kept)
	}
}

// TestClipToWireNeverSplitsARune. The clip walks the reasoning byte budget, and reasoning is the model's
// own prose in whatever language it was asked in — a cut mid-rune would put a replacement character into
// the transcript and, worse, into the JSON this relay sends.
func TestClipToWireNeverSplitsARune(t *testing.T) {
	const text = "asal sayılar: on yedi, on dokuz, yirmi üç — hepsi bölünmez"
	for budget := 0; budget <= len(text)+4; budget++ {
		got := clipToWire(text, budget)
		if !strings.HasPrefix(text, got) {
			t.Fatalf("clipToWire(budget=%d) = %q is not a prefix", budget, got)
		}
		if wireBytes(got) > budget {
			t.Fatalf("clipToWire(budget=%d) = %q costs %d wire bytes", budget, got, wireBytes(got))
		}
		if strings.ContainsRune(got, '�') {
			t.Fatalf("clipToWire(budget=%d) split a rune: %q", budget, got)
		}
	}
}

// TestClipToWireHoldsForABroadcastThatGrows is the case the second loop in clipToWire exists for, and it
// is the one a per-character cost model gets wrong: NeutralizeBroadcasts rewrites `<!` as five bytes,
// which is more than those two runes cost apart, so the running sum UNDERSHOOTS and a prefix that looked
// affordable is not.
func TestClipToWireHoldsForABroadcastThatGrows(t *testing.T) {
	const text = "the model wrote <!channel> into its reasoning"
	for budget := 0; budget <= wireBytes(text); budget++ {
		if got := clipToWire(text, budget); wireBytes(got) > budget {
			t.Fatalf("clipToWire(budget=%d) = %q costs %d wire bytes — the broadcast grew past the running sum",
				budget, got, wireBytes(got))
		}
	}
}

// TestATowerOfToolProgressNotificationsCannotBlankItsCard is the SAME ceiling, on the path that was
// already walking towards it before any of this existed.
//
// A tools/call progress message is capped at 4 KiB by the coordinator (maxProgressMessage) and
// maxCardLines admits twenty of them, so a long MCP tool could put 80 KB into one card's `details` — and
// what it would get for that is a card storing NOTHING, on twenty calls that each answered ok. Nothing
// had measured the wall; the shared budget is what stops both kinds of card at it.
func TestATowerOfToolProgressNotificationsCannotBlankItsCard(t *testing.T) {
	events := []palai.Event{shellStep("tc_1", "long running thing")}
	for i := 0; i < 20; i++ {
		events = append(events, palai.Event{Type: "tool_call.progress.v1",
			Data: map[string]any{"tool_call_id": "tc_1", "message": strings.Repeat("p", 4*1024)}})
	}
	events = append(events, doneStep("tc_1", "palai.workspace.shell"),
		palai.Event{Type: "run.completed.v1", Data: map[string]any{}})
	fake := runCards(t, events)

	if spent := wireSpend(fake.tasks, "tc_1"); spent > maxCardDetails {
		t.Fatalf("the tool card sent %d wire bytes of details, past the measured %d ceiling", spent, maxCardDetails)
	}
	var joined strings.Builder
	for _, task := range fake.tasks {
		joined.WriteString(task.Detail)
	}
	if !strings.Contains(joined.String(), cardTruncated) {
		t.Fatal("the tool card was cut by the byte budget and said nothing about it")
	}
}
