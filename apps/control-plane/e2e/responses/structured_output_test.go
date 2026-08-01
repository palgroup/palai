//go:build e2e

package responses

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// proseProvider answers every model step with one final PROSE answer and records the
// OutputSchema the control plane attached to each request it received.
//
// Recording the request is the whole point. A test that only looked at the run's outcome
// could not tell "the model was told to emit JSON and disobeyed" from "the model was never
// told anything" — and on 2026-08-01 it was the second one, for every run ever made against
// this stack. `output.format` was accepted by the API, folded into the idempotency hash, and
// then dropped: `grep -rn 'OutputSchema:' --include='*.go' . | grep -v _test` returned NOTHING,
// so nothing in production ever set this field. The adapters that send it downstream
// (provider_one/adapter.go:296 response_format, provider_two/adapter.go:390 output_config)
// have been shipped and unreachable. So the assertion is on the RECORDED REQUEST, not on the
// answer: the answer would be prose either way.
type proseProvider struct {
	mu      sync.Mutex
	schemas []*modelbroker.OutputSchema
	answer  string
}

func (p *proseProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.schemas = append(p.schemas, req.OutputSchema)
	p.mu.Unlock()
	return modelbroker.Result{
		ModelRequestID:    req.ModelRequestID,
		Model:             "fake",
		ProviderRequestID: "prov_final",
		Output:            p.answer,
		FinishReason:      "stop",
		Usage:             contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
		Attempts:          1,
	}, nil
}

func (p *proseProvider) recorded() []*modelbroker.OutputSchema {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*modelbroker.OutputSchema(nil), p.schemas...)
}

// citySchema is the demanded contract used by every case here: an object the prose answer
// below cannot possibly satisfy, so "did validation fire" has an unambiguous answer.
const citySchema = `{"type":"object","properties":{"city":{"type":"string"},"population":{"type":"integer"}},"required":["city","population"],"additionalProperties":false}`

const createWithSchema = `{"input":"Give the city of Ankara and its approximate population.",` +
	`"output":{"format":"json_schema","name":"city_fact","schema":` + citySchema + `,"strict":true}}`

// TestDemandedSchemaReachesTheProviderRequest is the first half of the defect: a caller demands
// a schema and the model is never told about it.
//
// RED on 7fd45ff0: the provider recorded OutputSchema=nil on every step — the field the caller
// filled travelled no further than the idempotency hash.
func TestDemandedSchemaReachesTheProviderRequest(t *testing.T) {
	h := newHarness(t)
	provider := &proseProvider{answer: "Ankara is the capital of Turkey, with about 5.7 million people."}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	responseID, _, _ := h.admitWith(createWithSchema, newID("idem"))
	h.awaitResponseTerminal(responseID, 60*time.Second)

	recorded := provider.recorded()
	if len(recorded) == 0 {
		t.Fatalf("provider saw no model request at all")
	}
	for i, got := range recorded {
		if got == nil {
			t.Fatalf("model request %d carried OutputSchema=nil: the API accepted output.format=json_schema "+
				"with a schema, hashed it into the idempotency record, and then told the model nothing. "+
				"The caller believes a schema was enforced; the model was never asked for one.", i)
		}
		if got.Name != "city_fact" || !got.Strict {
			t.Fatalf("model request %d carried OutputSchema{Name:%q, Strict:%v}, want {city_fact, true}", i, got.Name, got.Strict)
		}
		var want, have map[string]any
		_ = json.Unmarshal([]byte(citySchema), &want)
		raw, _ := json.Marshal(got.Schema)
		_ = json.Unmarshal(raw, &have)
		if !jsonEqual(want, have) {
			t.Fatalf("model request %d carried schema %s, want %s", i, raw, citySchema)
		}
	}
}

// TestOutputThatViolatesTheDemandedSchemaDoesNotComplete is the second half: even once the model
// IS told, a provider that cannot or does not comply must not yield a `completed` run. A run whose
// output does not satisfy the schema it was given has failed, and the caller must be able to see
// which schema and which output.
//
// RED on 7fd45ff0: state was `completed` and the projection carried the prose verbatim — the
// terminal state said the request succeeded while the contract the caller stated was unmet.
func TestOutputThatViolatesTheDemandedSchemaDoesNotComplete(t *testing.T) {
	h := newHarness(t)
	provider := &proseProvider{answer: "Ankara is the capital of Turkey, with about 5.7 million people."}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	responseID, _, _ := h.admitWith(createWithSchema, newID("idem"))
	h.awaitResponseTerminal(responseID, 60*time.Second)

	state, projection := h.response(responseID)
	if state == "completed" {
		t.Fatalf("state = completed with output %+v, which does not satisfy the demanded schema %s. "+
			"A run that was given a schema and returned prose has NOT completed — calling it completed "+
			"tells the caller their contract held when it did not.", projection.Output, citySchema)
	}
	if state != "failed" {
		t.Fatalf("state = %q, want failed", state)
	}
}

// TestSchemaFreeRunIsBitUnchanged is the regression fence around both of the above: a request that
// names no output format must reach the provider with OutputSchema=nil, exactly as before this
// feature existed. Structured output is opt-in, and the cost of adding it to every other run is zero.
func TestSchemaFreeRunIsBitUnchanged(t *testing.T) {
	h := newHarness(t)
	provider := &proseProvider{answer: "done"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	responseID, _, _ := h.admitWith(`{"input":"do the work"}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	for i, got := range provider.recorded() {
		if got != nil {
			t.Fatalf("model request %d of a schema-free run carried OutputSchema %+v, want nil", i, got)
		}
	}
}

// awaitResponseTerminal polls until the response reaches ANY terminal state, which is what these
// cases need: awaitResponseState would have to name the state up front, and the whole question here
// is WHICH terminal a schema-violating run reaches. It lives in this file rather than the shared
// harness so the sibling suites' helper set is untouched.
func (h *harness) awaitResponseTerminal(responseID string, within time.Duration) string {
	h.t.Helper()
	terminal := map[string]bool{"completed": true, "failed": true, "canceled": true, "timed_out": true, "budget_exceeded": true}
	deadline := time.Now().Add(within)
	var last string
	for time.Now().Before(deadline) {
		if last, _ = h.response(responseID); terminal[last] {
			return last
		}
		time.Sleep(25 * time.Millisecond)
	}
	h.t.Fatalf("response %s state = %q after %s, want any terminal state", responseID, last, within)
	return ""
}

// jsonEqual compares two decoded JSON documents structurally.
func jsonEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
