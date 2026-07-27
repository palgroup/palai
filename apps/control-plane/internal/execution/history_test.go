package execution

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal error = %v", err)
	}
	return string(b)
}

// TestHistoryMessagesRetainedPurgedAndPending proves run.start history assembly: a retained
// prior response carries its output as an assistant turn, a purged one collapses to a
// redacted_content marker (never its original content), and a prior with no output yet is
// skipped (spec §22.2; NO compaction).
func TestHistoryMessagesRetainedPurgedAndPending(t *testing.T) {
	prior := []coordinator.PriorResponse{
		{Output: []byte(`{"output":[{"type":"message","content":"12"}],"usage":{}}`)}, // retained
		{Purged: true},                    // purged: content reaped
		{Output: []byte(`{"output":[]}`)}, // terminal-less / no output yet
		{Output: nil},                     // queued prior, nothing stored
	}
	msgs := historyMessages(prior, defaultHistoryBudgetChars)
	if len(msgs) != 2 {
		t.Fatalf("history has %d messages, want 2 (empty/pending priors skipped): %v", len(msgs), msgs)
	}

	retained, ok := msgs[0].(map[string]any)
	if !ok || retained["role"] != "assistant" {
		t.Fatalf("first history message = %v, want an assistant turn", msgs[0])
	}
	if s := mustJSON(t, retained["content"]); !strings.Contains(s, "12") {
		t.Fatalf("retained history content = %s, want the prior output carried", s)
	}

	purged, ok := msgs[1].(map[string]any)
	if !ok || purged["role"] != "assistant" {
		t.Fatalf("second history message = %v, want an assistant turn", msgs[1])
	}
	if s := mustJSON(t, purged["content"]); !strings.Contains(s, "redacted_content") {
		t.Fatalf("purged history content = %s, want a redacted_content marker", s)
	}
}

// TestHistoryMessagesCarryTheQuestionsAndPlainText is the amnesia defect, and it is two defects with one
// cause. A real Slack thread chained four runs onto one session and the model still answered "sorry, I forgot
// your previous message" — because history carried only ASSISTANT turns, so the model could see its own
// answers and never the questions.
//
// The second half is what those assistant turns LOOKED like. Content was the stored output-ITEM ARRAY, and
// model_dispatch's asJSONString serialises a non-string, so the provider was handed
// `[{"content":"…","type":"message"}]` as the assistant's own words — three times over. On the fourth turn the
// model imitated the format and answered with that envelope, which is exactly what landed in the workspace
// (resp_7878547d…, live, 2026-07-27). slackRunInput already learned this on the INPUT side: a string is the
// only shape that can be a conversation turn.
func TestHistoryMessagesCarryTheQuestionsAndPlainText(t *testing.T) {
	prior := []coordinator.PriorResponse{
		{Input: []byte(`"2+2 kaç"`), Output: []byte(`{"output":[{"type":"message","content":"2 + 2 = 4."}]}`)},
	}
	msgs := historyMessages(prior, defaultHistoryBudgetChars)
	if len(msgs) != 2 {
		t.Fatalf("one prior turn assembled %d messages, want 2 (the question AND the answer): %v", len(msgs), msgs)
	}
	question, _ := msgs[0].(map[string]any)
	if question["role"] != "user" || question["content"] != "2+2 kaç" {
		t.Fatalf("first history message = %v, want the user's own question — an assistant-only history cannot answer \"what did I just ask\"", msgs[0])
	}
	answer, _ := msgs[1].(map[string]any)
	if answer["role"] != "assistant" {
		t.Fatalf("second history message = %v, want an assistant turn", msgs[1])
	}
	if answer["content"] != "2 + 2 = 4." {
		t.Fatalf("assistant history content = %#v, want the plain TEXT. Anything else is serialised by asJSONString "+
			"and the model is shown an envelope as its own prior words — which it then imitates", answer["content"])
	}
}
