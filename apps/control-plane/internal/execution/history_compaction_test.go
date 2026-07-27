package execution

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

// historyBytes is what the assembled history actually costs the provider request: the marshalled
// size of the messages run.start carries. It is the same measure compaction budgets against, so a
// test and the implementation cannot disagree about what "too big" means.
func historyBytes(t *testing.T, msgs []any) int {
	t.Helper()
	b, err := json.Marshal(msgs)
	if err != nil {
		t.Fatalf("marshal history: %v", err)
	}
	return len(b)
}

// longSession fabricates a session that accumulated `turns` question/answer exchanges of roughly
// `chars` characters each — a Slack thread that has been going for a while. The content is derived
// from the turn index alone, so every test in this file sees the same bytes.
func longSession(turns, chars int) []coordinator.PriorResponse {
	prior := make([]coordinator.PriorResponse, 0, turns)
	for i := range turns {
		question, _ := json.Marshal(fmt.Sprintf("turn %d: %s", i, strings.Repeat("q", chars)))
		answer, _ := json.Marshal(map[string]any{
			"output": []map[string]any{{"type": "message", "content": fmt.Sprintf("answer %d: %s", i, strings.Repeat("a", chars))}},
		})
		prior = append(prior, coordinator.PriorResponse{Input: question, Output: answer})
	}
	return prior
}

// TestHistoryStaysWithinTheContextBudget is the defect this task exists for, and until it was written
// NOTHING in the tree measured it: historyMessages returns every prior verbatim, SessionHistory's SQL
// has no LIMIT, and no budget is consulted anywhere. A Slack thread is a session, a session chains
// runs, and each run's history is every earlier turn — so the thread that works on Monday is the
// thread the provider refuses on Friday, with a raw upstream error and no diagnosis.
//
// 400 turns of ~1 KB is not a stress figure. It is a busy channel for a week.
func TestHistoryStaysWithinTheContextBudget(t *testing.T) {
	msgs := historyMessages(longSession(400, 1000), defaultHistoryBudgetChars)
	if got := historyBytes(t, msgs); got > defaultHistoryBudgetChars {
		t.Fatalf("assembled history is %d bytes, over the %d-byte budget: a long thread reaches the provider "+
			"as an oversized request and fails with an unclassified upstream error", got, defaultHistoryBudgetChars)
	}
}

// TestHistoryFoldsDeterministically is the constraint that ruled out a model-written summary. history.go's
// own contract is that a resumed attempt re-derives the SAME run.start; a summariser makes that false, and
// E10's replay claim goes with it. Folding twice must produce the same bytes because the fold is computed
// from the history itself.
func TestHistoryFoldsDeterministically(t *testing.T) {
	prior := longSession(200, 800)
	first := historyBytes(t, historyMessages(prior, 20000))
	firstJSON, _ := json.Marshal(historyMessages(prior, 20000))
	secondJSON, _ := json.Marshal(historyMessages(prior, 20000))
	if string(firstJSON) != string(secondJSON) {
		t.Fatalf("two folds of the same history differ:\nfirst  = %s\nsecond = %s", firstJSON, secondJSON)
	}
	if first == 0 {
		t.Fatal("folded history is empty; a fold must still carry the newest turns")
	}
}

// TestHistoryKeepsTheNewestTurnsVerbatim: the newest turns are the conversation. They pass through
// byte-identical to the uncompacted assembly — a window that paraphrases its own retained edge is a
// summariser wearing a windower's name.
func TestHistoryKeepsTheNewestTurnsVerbatim(t *testing.T) {
	prior := longSession(50, 500)
	folded := historyMessages(prior, 6000)
	full := historyMessages(prior, 1<<30)

	if len(folded) >= len(full) {
		t.Fatalf("history under a 6000-byte budget assembled %d messages, the unbudgeted one %d — nothing was folded", len(folded), len(full))
	}
	// The tail of the folded history must be the tail of the full history, message for message.
	tail := folded[1:] // [0] is the fold marker
	offset := len(full) - len(tail)
	for i, msg := range tail {
		got, _ := json.Marshal(msg)
		want, _ := json.Marshal(full[offset+i])
		if string(got) != string(want) {
			t.Fatalf("retained message %d was rewritten:\ngot  = %s\nwant = %s", i, got, want)
		}
	}
}

// TestHistoryFoldIsVisible: a silent cut is a lie. The model — and through it the person reading the
// answer — must be told that turns are gone, how many, and that nothing here stands in for them. This
// is a WINDOWER: the dropped content is not summarised, it is dropped, and the marker says so.
func TestHistoryFoldIsVisible(t *testing.T) {
	folded := historyMessages(longSession(50, 500), 6000)
	marker, ok := folded[0].(map[string]any)
	if !ok {
		t.Fatalf("first folded message = %#v, want the fold marker", folded[0])
	}
	text, _ := marker["content"].(string)
	if !strings.Contains(text, "dropped") {
		t.Fatalf("fold marker = %q, want it to say the turns were DROPPED — not summarised, which they were not", text)
	}
	if strings.Contains(strings.ToLower(text), "summar") && !strings.Contains(text, "nothing here summarises") {
		t.Fatalf("fold marker = %q claims to summarise; this is a WINDOWER and the content is gone", text)
	}
	// The count is derived, not hard-coded: whatever the fold retained, the marker must account for
	// the rest. A reader who cannot tell HOW MUCH is missing has not been told anything.
	dropped := 50 - (len(folded)-1)/2
	if !strings.Contains(text, strconv.Itoa(dropped)) {
		t.Fatalf("fold marker = %q, want the count of dropped turns (%d of 50)", text, dropped)
	}
}

// TestHistoryUnderBudgetIsUnchanged is the regression half: a conversation that fits pays nothing for
// this task. Its assembled history must be BIT-IDENTICAL to the pre-compaction path — no marker, no
// reordering, no re-encoding — so every existing run's run.start is unchanged.
func TestHistoryUnderBudgetIsUnchanged(t *testing.T) {
	prior := []coordinator.PriorResponse{
		{Input: []byte(`"2+2 kaç"`), Output: []byte(`{"output":[{"type":"message","content":"2 + 2 = 4."}]}`)},
		{Purged: true},
	}
	got, _ := json.Marshal(historyMessages(prior, defaultHistoryBudgetChars))
	want := `[{"content":"2+2 kaç","role":"user"},{"content":"2 + 2 = 4.","role":"assistant"},{"content":[{"type":"redacted_content"}],"role":"assistant"}]`
	if string(got) != want {
		t.Fatalf("a history that fits was rewritten:\ngot  = %s\nwant = %s", got, want)
	}
}

// TestHistoryFoldsWhenTheBudgetIsUnknown is the fail-closed rule. A budget of zero — no route resolved,
// no window known — must FOLD, not pass everything through. Not folding is the failure mode this task
// exists to remove, so an unknown budget takes the safe side.
func TestHistoryFoldsWhenTheBudgetIsUnknown(t *testing.T) {
	prior := longSession(400, 1000) // the 833 KB history of the RED case
	folded := historyMessages(prior, 0)
	if len(folded) >= len(historyMessages(prior, 1<<30)) {
		t.Fatal("an unknown (zero) budget passed the whole history through; fail-closed means fold")
	}
	if got := historyBytes(t, folded); got > defaultHistoryBudgetChars {
		t.Fatalf("unknown-budget history is %d bytes, over the conservative default %d", got, defaultHistoryBudgetChars)
	}
	// And it folds to EXACTLY the default, not to some other private ceiling.
	unknown, _ := json.Marshal(folded)
	explicit, _ := json.Marshal(historyMessages(prior, defaultHistoryBudgetChars))
	if string(unknown) != string(explicit) {
		t.Fatal("an unknown budget folded differently from the conservative default it claims to use")
	}
}
