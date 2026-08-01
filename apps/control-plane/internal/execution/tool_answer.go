package execution

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"

	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// This file is the orchestrator half of the answer/fault distinction drawn in
// packages/tool-broker/answer.go: the SHAPE a refusal reaches the model in, and the BOUND on how many
// of them one run may be handed.

// errToolAnswerBudget ends an attempt whose model has been handed more tool refusals than the budget
// allows. It is a SENTINEL on the run loop's tool.request switch, beside errToolUncertainWait and
// errRunParked — the same idiom, for the same reason: the two ends agree without parsing each other's
// text. The loop turns it into ONE named terminal failure (failToolAnswerBudget), never a retry.
var errToolAnswerBudget = errors.New("tool_answer_budget_exhausted")

// defaultToolAnswerErrorBudget bounds how many tool ERROR results one attempt may hand a model.
//
// WHY A BOUND HAD TO COME WITH THE FIX. Before it, a tool error ended the run — badly, but it ended it.
// Delivering the error as a result removes that accidental stop, so a model that does not learn from
// "no such file" can now ask again, and again. NOTHING ELSE IN THIS TREE WOULD STOP IT: there is no
// per-run step limit anywhere in the orchestrator or the reference engine, the only wall clock is the
// §20.12 ADMISSION queue deadline (which stops applying the moment a run starts), packages/model-broker's
// Budget says in its own doc comment that it is "NOT wired into the live dispatch path", and
// `max_tool_calls` is published in protocols/schemas/execution/response-create.json and typed in both
// SDKs with NO READER ANYWHERE — measured, not assumed:
//
//	grep -rn 'MaxToolCalls' --include='*.go' . | wc -l   ->  2   (2026-08-01)
//	    sdks/go/types.go:327  and  packages/contracts/response-create.gen.go:17 — two DECLARATIONS,
//	    zero uses. The field a caller would reach for to bound this does not do anything yet.
//
// So this budget is the bound, and it is deliberately narrow: it counts REFUSALS, not tool calls. A run
// making progress — reading files that exist, running commands that work — is not bounded by it at all,
// which is the honest scope. A run whose model has been told "no" sixteen times is not exploring.
//
// SIXTEEN, and the number is a judgement rather than a measurement, so it says so. An agent that guesses
// a path, lists the directory, and guesses again spends two or three; a session long enough to spend
// sixteen and still be wrong is one an operator should see fail with a reason rather than watch burn
// tokens. PALAI_TOOL_ERROR_BUDGET moves it, and 0 IS UNBOUNDED AND MUST BE WRITTEN — the idiom this tree
// already uses for PALAI_BACKGROUND_MAX_WALL_TIME, so "unbounded" is always somebody's decision on a line
// rather than a variable nobody set.
const defaultToolAnswerErrorBudget = 16

// toolAnswerErrorBudget reads the deployment's budget. It reads the environment at CALL time rather than
// at composition, which is the discipline main.go's background-ceiling comment states out loud: a value
// captured at the composition root is out of reach of every test that builds its own Orchestrator, and
// that is exactly how the shell wall time came to be unbounded on the host while every sandbox test was
// green. A malformed or negative value falls back to the default rather than silently unbounding.
func toolAnswerErrorBudget() int {
	raw, set := os.LookupEnv("PALAI_TOOL_ERROR_BUDGET")
	if !set {
		return defaultToolAnswerErrorBudget
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return defaultToolAnswerErrorBudget
	}
	return n // 0 = unbounded, and somebody typed it
}

// ToolErrorBudget exposes the reader the dispatcher actually calls, and it exists for exactly one
// caller: the composition root's TestEveryDesiredValueThisBinaryAcceptsIsParsedByItsOwnReader, which
// drives every value the write surface ACCEPTS through the REAL reader. That guard is the one that
// stops the panel showing one number while the process runs another, and it can only do its job if the
// reader is reachable from where it lives. A test-local copy of the parse would be the defect it fences.
func ToolErrorBudget() int { return toolAnswerErrorBudget() }

// exceedsToolAnswerErrorBudget reports whether a count has passed the deployment's budget.
func exceedsToolAnswerErrorBudget(count int) bool {
	budget := toolAnswerErrorBudget()
	return budget > 0 && count > budget
}

// answerResult renders a tool's refusal into the result the model is handed and the ledger records. It
// is ONE object for both, because they must not be able to differ: an operator reading tool_calls sees
// exactly the sentence the model was answering.
//
//	{"status":"error","error":{"code":"not_found","tool":"palai.workspace.file","message":"…"}}
//
// `status` is the discriminator a model reads; the nested `error.code` is the stable machine key, and it
// is nested rather than flat so isAnswerResult cannot mistake a SUCCESSFUL result that happens to carry
// a `status` field for a refusal — the background shell call returns exactly that shape
// ({task_id, output_path, status}) and would otherwise be miscounted against the budget.
//
// THE MESSAGE GOES THROUGH BOTH REDACTORS. It is about to be written to the tool ledger and read by a
// model, which are the same two destinations a shell result reaches, so it gets the same treatment: the
// shape-based scan for provider keys and bearer tokens, and the VALUE-based scan for this attempt's own
// environment values. Without the second one a tool error that echoed a credential — "connect to
// postgres://user:<password>@…" is the everyday form — would land verbatim in a durable row.
func answerResult(name string, answer *toolbroker.AnswerError, env toolbroker.ExecEnv) map[string]any {
	message := toolbroker.RedactSecrets(answer.Error())
	message = toolbroker.RedactValues(message, toolbroker.EnvValueList(env.EnvValues))
	return map[string]any{
		"status": "error",
		"error": map[string]any{
			"code":    answer.Code,
			"tool":    name,
			"message": message,
		},
	}
}

// isAnswerResult reports whether a COMMITTED tool result is one of answerResult's refusals, so a replay
// counts against the budget the fresh delivery counted against. It parses the JSON and reads the two
// fields answerResult writes; it never substring-matches, because a tool's own successful output can
// contain any bytes at all — including the word "error" — and this tree's history is that every
// comparison made on text rather than on structure has eventually been defeated.
func isAnswerResult(committed string) bool {
	var probe struct {
		Status string `json:"status"`
		Error  *struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(committed), &probe); err != nil {
		return false
	}
	return probe.Status == "error" && probe.Error != nil && probe.Error.Code != ""
}

// toolAnswerBudgetProblem is the terminal Response error an over-budget run projects. It is its own
// problem rather than the generic "Internal error" for the reason contextOverflowProblem gives one file
// over: Slack's failure line appends the problem TITLE, so this title is the difference between a person
// being told their agent got stuck in a loop and a person being told nothing. Status 400 — the run asked
// for something impossible over and over, which is a caller-side fact — and NOT retryable, because the
// same conversation replayed makes the same calls.
func toolAnswerBudgetProblem(count int) contracts.Problem {
	return contracts.Problem{
		Type:   problemTypePrefix + "tool_error_budget_exhausted",
		Code:   "tool_error_budget_exhausted",
		Title:  "The run kept calling a tool that kept refusing",
		Status: 400,
		Detail: fmt.Sprintf("the model was handed %d tool error results in this run (budget %d) and went on "+
			"calling anyway; the refusals are recorded on the run's tool calls — read them, then adjust the "+
			"instructions or the tool set, or raise PALAI_TOOL_ERROR_BUDGET", count, toolAnswerErrorBudget()),
	}
}
