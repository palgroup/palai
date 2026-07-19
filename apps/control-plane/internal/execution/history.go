package execution

import (
	"encoding/json"

	"github.com/palgroup/palai/packages/coordinator"
)

// historyMessages turns a session's prior responses into run.start conversation history
// (spec §9, §22.2): each retained response is an assistant turn carrying its output, a
// purged response collapses to a redacted_content marker (its content is gone), and a prior
// with no output yet is skipped. No compaction — the assembled turns are verbatim and
// deterministic, so a resumed attempt re-derives the same run.start.
func historyMessages(prior []coordinator.PriorResponse) []any {
	var msgs []any
	for _, p := range prior {
		if p.Purged {
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "redacted_content"}},
			})
			continue
		}
		content := priorOutput(p.Output)
		if content == nil {
			continue // not terminal yet: nothing to carry
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": content})
	}
	return msgs
}

// priorOutput extracts the output content of a stored response projection, or nil if the
// projection has no output (a still-running prior) or an empty one.
func priorOutput(projection []byte) any {
	if len(projection) == 0 {
		return nil
	}
	var p struct {
		Output json.RawMessage `json:"output"`
	}
	if err := json.Unmarshal(projection, &p); err != nil || len(p.Output) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(p.Output, &out); err != nil {
		return nil
	}
	if arr, ok := out.([]any); ok && len(arr) == 0 {
		return nil
	}
	return out
}
