package execution

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/palgroup/palai/packages/coordinator"
)

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

// RED: 400 turns of ~1 KB is a busy channel for a week, not a stress figure.
func TestHistoryStaysWithinTheContextBudget(t *testing.T) {
	const budget = 120000
	msgs := historyMessages(longSession(400, 1000))
	b, _ := json.Marshal(msgs)
	if len(b) > budget {
		t.Fatalf("assembled history is %d bytes, over the %d-byte budget", len(b), budget)
	}
}
