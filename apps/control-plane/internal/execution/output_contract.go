package execution

import (
	"encoding/json"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/outputcontract"
)

// parseOutputContract decodes the run's stored §22.7 output contract (runs.output_contract,
// migration 000052). A NULL/empty column is the zero Contract — no format named, nothing demanded,
// which is what every run that does not opt in carries.
//
// It re-runs outputcontract.Check on the stored schema rather than trusting the column. The value
// was written by the API only after Parse accepted it, so this is belt-and-braces — but the column
// is the input to the ONE decision that must never fail open (whether the model is constrained and
// the answer checked), and a schema that reached the row by any other path must not be honoured
// silently. A stored document this server would not accept today is an error, not a relaxation.
func parseOutputContract(raw []byte) (outputcontract.Contract, error) {
	if len(raw) == 0 {
		return outputcontract.Contract{Format: outputcontract.FormatText}, nil
	}
	var stored map[string]any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return outputcontract.Contract{}, fmt.Errorf("decode stored output contract: %w", err)
	}
	contract, err := outputcontract.Parse(stored)
	if err != nil {
		return outputcontract.Contract{}, fmt.Errorf("stored output contract is not one this server accepts: %w", err)
	}
	return contract, nil
}

// brokerOutputSchema renders the run's contract as the model broker's own constraint, or nil when
// the run demands nothing.
//
// THIS FUNCTION IS THE WRITER THE FEATURE TURNED ON. modelbroker.Request.OutputSchema has existed
// since the broker was written and both direct adapters have always forwarded it —
// provider_one/adapter.go:296 renders it as OpenAI's response_format.json_schema,
// provider_two/adapter.go:390 as Anthropic's output_config.format, and
// openai_compatible/adapter.go:114 REFUSES the call outright when the probed endpoint does not
// advertise strict JSON. Measured on 2026-08-01, `grep -rn 'OutputSchema:' --include='*.go' .`
// outside tests matched no broker request anywhere: the entire downstream half was shipped,
// correct, and unreachable, because nothing in production ever set the field. One missing writer
// is the whole of what made a published request field a no-op.
func brokerOutputSchema(c outputcontract.Contract) *modelbroker.OutputSchema {
	if !c.Demanded() {
		return nil
	}
	return &modelbroker.OutputSchema{Name: c.Name, Schema: c.Schema, Strict: c.Strict}
}

// schemaValidationProblem is the RFC 9457 problem a run fails with when its answer does not satisfy
// the schema the request demanded (spec §22.7's schema_validation_failed).
//
// It is NOT the generic terminal `failed` problem, and the difference is the point: "the run failed
// during execution" tells a caller nothing they can act on, while "output.population is missing"
// tells them whether to fix their schema, change their prompt, or retry. Retryable is true because a
// model is stochastic — the same request may well conform next time, and a retry is exactly what
// §22.7's bounded repair would have automated.
func schemaValidationProblem(detail string) *contracts.Problem {
	return &contracts.Problem{
		Type:      contracts.ProblemTypePrefix + "schema_validation_failed",
		Code:      "schema_validation_failed",
		Title:     "Output did not satisfy the requested schema",
		Status:    422,
		Detail:    detail,
		Retryable: true,
	}
}

// validateTerminalOutput checks a completed run's answer against the schema its request demanded,
// returning the problem it violates or nil when the run demanded nothing or the answer conforms.
//
// It reads the SAME bytes the projection is about to publish — st.output, falling back to the
// terminal frame's own output exactly as finalize does — so it cannot pass a document and then
// store a different one.
func validateTerminalOutput(st *attemptState, frame contracts.EngineFrame) *contracts.Problem {
	if !st.outputContract.Demanded() {
		return nil
	}
	text, ok := terminalOutputText(st, frame)
	if !ok {
		return schemaValidationProblem("a schema was demanded but the run produced no text answer to validate")
	}
	if err := outputcontract.ValidateText(st.outputContract.Schema, text); err != nil {
		return schemaValidationProblem(err.Error())
	}
	return nil
}

// terminalOutputText extracts the run's final text answer, mirroring finalize's own projection
// choice: the accumulated output items, or — when the engine emitted none — the terminal frame's
// own output value.
func terminalOutputText(st *attemptState, frame contracts.EngineFrame) (string, bool) {
	for _, item := range st.output {
		if content, ok := item["content"].(string); ok {
			return content, true
		}
	}
	if value, present := frame.Data["output"]; present {
		if content, ok := value.(string); ok {
			return content, true
		}
	}
	return "", false
}

// outputContractFrame renders the contract as run.start's data.output_contract, or nil when the run
// demands nothing — in which case the key is omitted and the frame stays byte-identical to the
// pre-§22.7 one. The shape is pinned by protocols/engine/engine.schema.json; the reference engine
// reads it in loop.py:_finish.
func outputContractFrame(c outputcontract.Contract) map[string]any {
	if !c.Demanded() {
		return nil
	}
	return map[string]any{
		"format": c.Format,
		"name":   c.Name,
		"schema": c.Schema,
		"strict": c.Strict,
	}
}
