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
	msgs := historyMessages(prior)
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
