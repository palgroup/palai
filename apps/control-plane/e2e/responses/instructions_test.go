//go:build e2e

package responses

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// recordingProvider answers every model step with one final answer and records the MESSAGES the
// control plane put on the provider wire.
//
// Recording the messages is the whole point, and it is the only instrument that can tell the two
// candidate explanations apart. A test that looked at the ANSWER could not distinguish "the model
// was told to answer in one word and ignored it" from "the model was never told anything" — and
// measured against a real provider on 2026-08-01 it was the second one, for every run this stack
// has ever made:
//
//	no agent, no instructions                               -> usage.input_tokens 48
//	instructions: "Her zaman tek kelime cevapla."            -> 48
//	agent_revision_id (whose revision carries instructions)  -> 48
//	agent_id (resolving to that same published revision)     -> 48
//	instructions: ~320 words                                 -> 48
//
// A 320-word instruction added ZERO tokens. The instrument was sound: the same 320 words placed in
// the INPUT measured 689 tokens, and a one-word instruction placed in the INPUT was obeyed
// ("Ankara."). So the field was accepted, hashed into the idempotency record, and dropped — and the
// pinned revision's `instructions` column, which agent CRUD has stored since E11, was read by no
// query on the execution path at all.
//
// This suite runs the REAL reference engine as a subprocess (subprocessDialer), so the messages
// recorded here are the ones a real engine actually produced, not a fabrication of a fake.
type recordingProvider struct {
	mu       sync.Mutex
	requests [][]modelbroker.Message
	answer   string
}

func (p *recordingProvider) Execute(_ context.Context, req modelbroker.Request, _ string, _ func(modelbroker.Delta)) (modelbroker.Result, error) {
	p.mu.Lock()
	p.requests = append(p.requests, append([]modelbroker.Message(nil), req.Messages...))
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

// firstRequest returns the messages of the first model request the provider saw.
func (p *recordingProvider) firstRequest(t *testing.T) []modelbroker.Message {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) == 0 {
		t.Fatalf("provider saw no model request at all")
	}
	return p.requests[0]
}

// systemTexts returns the content of every system-role message, in wire order. The instruction
// layers of spec §25.12 are system turns, so this is the list whose ORDER the layering claim is about.
func systemTexts(messages []modelbroker.Message) []string {
	var out []string
	for _, m := range messages {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

// indexOfText reports the position in `messages` of the first message CONTAINING needle, or -1.
// Position, not presence, is what the §25.12 ordering claim needs.
func indexOfText(messages []modelbroker.Message, needle string) int {
	for i, m := range messages {
		if strings.Contains(m.Content, needle) {
			return i
		}
	}
	return -1
}

const (
	revisionInstruction = "Yalnizca tek kelime ile cevapla."
	requestInstruction  = "Cevabini buyuk harfle yaz."
)

// publishRevisionWithInstructions creates and publishes an agent revision carrying `instructions`,
// returning the revision id.
func (h *harness) publishRevisionWithInstructions(t *testing.T, body string) string {
	t.Helper()
	st, profile := h.postAgent("/v1/agents", `{"name":"instructed"}`)
	if st != http.StatusCreated {
		t.Fatalf("create profile status = %d, want 201", st)
	}
	profileID, _ := profile["id"].(string)
	st, rev := h.postAgent("/v1/agents/"+profileID+"/revisions", body)
	if st != http.StatusCreated {
		t.Fatalf("create revision status = %d, want 201 (body %s)", st, body)
	}
	revID, _ := rev["id"].(string)
	if st, _ := h.postAgent("/v1/agents/"+profileID+"/revisions/"+revID+"/publish", ``); st != http.StatusOK {
		t.Fatalf("publish revision status = %d, want 200", st)
	}
	return revID
}

// TestPinnedRevisionInstructionsReachTheProviderRequest is the defect, stated as the smallest thing
// that can be wrong: a caller pins a revision that says "answer in one word", and the model is never
// told.
//
// RED on eec2d7fd: the provider's messages were [kernel system, user input] — the revision's
// instructions appeared nowhere. The run recorded the pin (runs.agent_revision_id was correctly
// non-NULL), so every observable EXCEPT the one that matters said the agent had been applied.
func TestPinnedRevisionInstructionsReachTheProviderRequest(t *testing.T) {
	h := newHarness(t)
	provider := &recordingProvider{answer: "Ankara"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	revID := h.publishRevisionWithInstructions(t, `{"instructions":"`+revisionInstruction+`"}`)

	responseID, _, _ := h.admitWith(`{"input":"Turkiye'nin baskenti neresidir?","agent_revision_id":"`+revID+`"}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	messages := provider.firstRequest(t)
	if indexOfText(messages, revisionInstruction) < 0 {
		t.Fatalf("the pinned revision's instructions reached no message on the provider wire.\n"+
			"  system turns seen: %q\n"+
			"The run pinned the revision, agent CRUD stored the text, and the model was told none of "+
			"it. Agent configuration that records a pin and applies nothing is decorative.",
			systemTexts(messages))
	}
}

// TestRequestInstructionsReachTheProviderRequest is the same defect on the request-level field —
// the one the PUBLISHED schema has always declared (response-create.json `instructions`).
//
// RED on eec2d7fd: absent from every message. A caller could read the schema, send the field,
// receive 202, and get a run that never saw it.
func TestRequestInstructionsReachTheProviderRequest(t *testing.T) {
	h := newHarness(t)
	provider := &recordingProvider{answer: "ANKARA"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	responseID, _, _ := h.admitWith(`{"input":"Turkiye'nin baskenti neresidir?","instructions":"`+requestInstruction+`"}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	messages := provider.firstRequest(t)
	if indexOfText(messages, requestInstruction) < 0 {
		t.Fatalf("request-level `instructions` reached no message on the provider wire.\n"+
			"  system turns seen: %q\n"+
			"The field is in the published schema and was accepted with 202.", systemTexts(messages))
	}
}

// TestInstructionLayersComposeInSpecOrder pins the PRECEDENCE decision, which is the part a caller
// has to be able to predict.
//
// Spec §25.12 states the context layers as an ordered stack, not a contest:
//
//	1. kernel safety and protocol instructions
//	2. deployment/organization/project policy-visible instructions   (no writer yet)
//	3. pinned agent revision instructions
//	4. session config instructions                                    (no writer yet)
//	5. run-specific instructions
//	6. selected durable conversation items
//	10. current user/trigger input
//
// So they COMPOSE, and the revision (3) precedes the request (5). Replacement was the other
// candidate and it is the wrong one here: it would let any request that happens to carry
// `instructions` silently strip the guardrails of the agent it pinned, which is a capability
// EXPANSION of exactly the kind the pinned revision's tool ceiling exists to prevent
// (execution/config.go:49-51, spec §63.4 "capability never expands").
func TestInstructionLayersComposeInSpecOrder(t *testing.T) {
	h := newHarness(t)
	provider := &recordingProvider{answer: "ANKARA"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	revID := h.publishRevisionWithInstructions(t, `{"instructions":"`+revisionInstruction+`"}`)

	responseID, _, _ := h.admitWith(`{"input":"Turkiye'nin baskenti neresidir?",`+
		`"agent_revision_id":"`+revID+`","instructions":"`+requestInstruction+`"}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	messages := provider.firstRequest(t)
	revIdx := indexOfText(messages, revisionInstruction)
	reqIdx := indexOfText(messages, requestInstruction)
	if revIdx < 0 || reqIdx < 0 {
		t.Fatalf("composition dropped a layer: revision at %d, request at %d.\n  system turns seen: %q\n"+
			"A run that pins a revision AND names instructions must carry BOTH — dropping either one is a "+
			"silent loss of something the caller stated.", revIdx, reqIdx, systemTexts(messages))
	}
	if revIdx >= reqIdx {
		t.Fatalf("layer order is revision@%d, request@%d — §25.12 puts the pinned revision (layer 3) "+
			"BEFORE run-specific instructions (layer 5).\n  system turns seen: %q", revIdx, reqIdx, systemTexts(messages))
	}
	// The whole instruction stack sits above the conversation: after the kernel layer, before the
	// user's input (§25.12 layers 1 < 3,5 < 10).
	inputIdx := indexOfText(messages, "Turkiye'nin baskenti neresidir?")
	if inputIdx < 0 {
		t.Fatalf("the user input reached no message at all; messages = %+v", messages)
	}
	if reqIdx >= inputIdx {
		t.Fatalf("instructions landed at %d, AFTER the user input at %d — §25.12 puts every instruction "+
			"layer above the conversation", reqIdx, inputIdx)
	}
	if len(messages) == 0 || messages[0].Role != "system" || strings.Contains(messages[0].Content, revisionInstruction) {
		t.Fatalf("message 0 = %+v, want the engine's KERNEL system turn — §25.12 layer 1 outranks "+
			"every caller-supplied layer, so no caller instruction may be placed above it", messages[0])
	}
}

// TestInstructionFreeRunIsBitUnchanged is the regression fence. A run that names no instructions and
// pins no revision must reach the provider with EXACTLY the message list it had before this feature:
// the engine's kernel turn and the user input, and nothing else. Instructions are opt-in and the
// cost to every other run is zero.
func TestInstructionFreeRunIsBitUnchanged(t *testing.T) {
	h := newHarness(t)
	provider := &recordingProvider{answer: "done"}
	stop := h.runWorker(h.newOrchestratorWithAdapter(subprocessDialer{engineDir: h.engineDir}, provider))
	defer stop()

	responseID, _, _ := h.admitWith(`{"input":"do the work"}`, newID("idem"))
	h.awaitResponseState(responseID, "completed", 60*time.Second)

	messages := provider.firstRequest(t)
	if got := systemTexts(messages); len(got) != 1 {
		t.Fatalf("an instruction-free run carried %d system turns %q, want exactly 1 (the engine kernel). "+
			"An opt-in layer that appears when nothing opted in is a context-budget regression on every "+
			"run in the deployment.", len(got), got)
	}
	if len(messages) != 2 {
		t.Fatalf("an instruction-free run carried %d messages, want 2 (kernel system + user input): %+v",
			len(messages), messages)
	}
}
