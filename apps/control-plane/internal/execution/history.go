package execution

import (
	"encoding/json"

	"github.com/palgroup/palai/packages/coordinator"
)

// historyMessages turns a session's prior responses into run.start conversation history
// (spec §9, §22.2): each retained response contributes the QUESTION it was asked and the
// ANSWER it produced, a purged response collapses to a redacted_content marker (its content
// is gone), and a prior with no output yet is skipped. No compaction — the assembled turns
// are verbatim and deterministic, so a resumed attempt re-derives the same run.start.
//
// TWO THINGS HERE ARE LOAD BEARING, and both were learned from one real conversation that
// forgot itself four runs deep:
//
//  1. THE USER TURNS ARE CARRIED. Assistant turns alone are replies with nothing they were
//     replies to: the model can read what it said and never what it was asked, so "what did I
//     just ask you" is genuinely unanswerable. It answered exactly that way, in Turkish, in a
//     live workspace, in a thread whose four runs all correlated correctly.
//  2. THE ASSISTANT TURN IS PLAIN TEXT, never the stored output-ITEM array. model_dispatch's
//     asJSONString passes a string through untouched and SERIALISES anything else, so an array
//     arrived at the provider as `[{"content":"…","type":"message"}]` presented as the
//     assistant's own prior words. A model shown three of those imitated the format, and the
//     fourth answer WAS that envelope — posted verbatim into the workspace. slackRunInput
//     already carries the same rule for the current turn, in as many words: a string is the
//     only shape that can be a conversation turn.
func historyMessages(prior []coordinator.PriorResponse) []any {
	var msgs []any
	for _, p := range prior {
		if p.Purged {
			// The content is reaped — the question with it. Nothing may be reconstructed from it.
			msgs = append(msgs, map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "redacted_content"}},
			})
			continue
		}
		answer := outputText(p.Output)
		if answer == "" {
			continue // not terminal yet, or nothing said: neither half of the exchange is settled
		}
		if question := priorInput(p.Input); question != nil {
			msgs = append(msgs, map[string]any{"role": "user", "content": question})
		}
		msgs = append(msgs, map[string]any{"role": "assistant", "content": answer})
	}
	return msgs
}

// priorInput decodes a stored response's input, or nil when there is none. The value is passed
// on in the SHAPE IT WAS STORED IN — exactly what run.start does with the CURRENT turn's input
// — so a prior turn and the present one are described to the engine the same way.
func priorInput(input []byte) any {
	if len(input) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(input, &value); err != nil {
		return nil
	}
	return value
}
