package execution

import (
	"encoding/json"
	"strconv"

	"github.com/palgroup/palai/packages/coordinator"
)

// defaultHistoryBudgetChars is the assembled-history ceiling in MARSHALLED BYTES, and the number is
// an estimate rather than a measurement of anything the provider counts.
//
// Where it comes from: ~3 bytes per token on the mixed Turkish/English/code this system actually
// carries puts 120 000 bytes at roughly 40 000 tokens — comfortably inside the 128k window of the
// deployment-default model (gpt-4o-mini, main.go's PALAI_MODEL default) with room left for the
// current turn's input, any advertised tool schemas, and the answer itself.
//
// It is a CONSTANT and not a per-model lookup because there is nothing to look up: measured against
// the tree on 2026-07-27, no context window is stored anywhere — not on model_routes, not in the
// broker, not in either provider adapter. A per-model table would be invented, not resolved. When a
// route revision starts carrying a window, this becomes a lookup with this value as its fallback.
//
// HONEST CEILING: bytes are not tokens. A history that fits this budget can still overflow a real
// tokenizer at the margin, which is why classifyContextOverflow (orchestrator.go) still exists.
const defaultHistoryBudgetChars = 120000

// minRetainedTurns is how many of the newest exchanges survive folding no matter what. One, because
// a run that carries none of the conversation is a run that cannot answer the question it was asked.
// A single turn larger than the whole budget therefore still exceeds it — deliberately: dropping
// everything would be worse, and the typed context-overflow failure is the net under that case.
const minRetainedTurns = 1

// historyMessages turns a session's prior responses into run.start conversation history
// (spec §9, §22.2): each retained response contributes the QUESTION it was asked and the
// ANSWER it produced, a purged response collapses to a redacted_content marker (its content
// is gone), and a prior with no output yet is skipped.
//
// COMPACTION IS A WINDOW, NOT A SUMMARY, and the name is the honest one. When the assembled
// history exceeds `budget` marshalled bytes, the OLDEST turns are dropped and replaced by one
// marker that says how many went; the newest turns pass through byte-identical. Nothing
// paraphrases the dropped turns, so information is genuinely LOST — visibly, which is the whole
// point: a person who cannot tell what the agent forgot is back in the state this task was opened
// to end.
//
// A model-written summary was the obvious alternative and it is REFUSED here, on this file's own
// terms: the fold must be derived from the history itself so that a resumed attempt re-derives the
// same run.start (see below). A summariser makes that false and takes E10's replay claim with it.
//
// A budget of zero or less means "unknown", and an unknown budget FOLDS at the conservative
// default rather than passing everything through — not folding is the failure mode this exists to
// remove. A history that fits its budget is assembled exactly as it was before compaction existed:
// no marker, byte-for-byte the old shape.
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
func historyMessages(prior []coordinator.PriorResponse, budget int) []any {
	// One turn's messages stay together: a question is folded away with its own answer, never
	// stranded as a user message whose reply is gone.
	turns := make([][]any, 0, len(prior))
	for _, p := range prior {
		if p.Purged {
			// The content is reaped — the question with it. Nothing may be reconstructed from it.
			turns = append(turns, []any{map[string]any{
				"role":    "assistant",
				"content": []any{map[string]any{"type": "redacted_content"}},
			}})
			continue
		}
		answer := outputText(p.Output)
		if answer == "" {
			continue // not terminal yet, or nothing said: neither half of the exchange is settled
		}
		var turn []any
		if question := priorInput(p.Input); question != nil {
			turn = append(turn, map[string]any{"role": "user", "content": question})
		}
		turns = append(turns, append(turn, map[string]any{"role": "assistant", "content": answer}))
	}
	return foldToBudget(turns, budget)
}

// foldToBudget keeps the newest turns that fit and replaces the rest with one visible marker. The
// decision is a walk backwards over the turns' own marshalled sizes — no clock, no randomness, no
// model — so the same history always folds to the same bytes.
func foldToBudget(turns [][]any, budget int) []any {
	if budget <= 0 {
		budget = defaultHistoryBudgetChars // unknown budget: fold rather than not fold
	}
	sizes := make([]int, len(turns))
	total := 2 // the enclosing [ ]
	for i, turn := range turns {
		for _, msg := range turn {
			encoded, err := json.Marshal(msg)
			if err != nil {
				// Unmarshalable content cannot be budgeted; treat it as expensive so it folds first.
				sizes[i] += budget
				continue
			}
			sizes[i] += len(encoded) + 1 // + the separating comma
		}
		total += sizes[i]
	}

	var msgs []any
	if total <= budget {
		for _, turn := range turns {
			msgs = append(msgs, turn...)
		}
		return msgs // fits: assembled exactly as it was before compaction existed
	}

	// Walk from the newest turn backwards, spending the budget the fold marker leaves behind.
	remaining := budget - len(foldMarkerText(len(turns))) - 32 // 32: the marker's own JSON envelope
	kept := 0
	for i := len(turns) - 1; i >= 0; i-- {
		if kept >= minRetainedTurns && remaining-sizes[i] < 0 {
			break
		}
		remaining -= sizes[i]
		kept++
	}

	msgs = append(msgs, map[string]any{"role": "assistant", "content": foldMarkerText(len(turns) - kept)})
	for _, turn := range turns[len(turns)-kept:] {
		msgs = append(msgs, turn...)
	}
	return msgs
}

// foldMarkerText is what stands where the dropped turns were. It says DROPPED and not "summarised"
// because that is what happened: a silent cut is a lie, and so is a marker that implies the content
// survived in some compressed form. It is addressed to the model because the model is what reads
// run.start — and a model told plainly that it is missing context asks for it instead of inventing it.
func foldMarkerText(dropped int) string {
	return "[" + strconv.Itoa(dropped) + " earlier turns of this conversation were dropped to fit the model's " +
		"context window. Their content is gone and nothing here summarises it. If you need something from " +
		"before this point, ask.]"
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
