//go:build live

// Package provider_test's provider-two leg is the REAL second-provider smoke. It runs
// only under the `live` build tag, in `make test-live-provider PROVIDER=provider-two`,
// which loads the real Anthropic credential from .env.local into the environment at
// runtime. It proves the provider-two adapter converts a REAL streamed Anthropic message
// into a canonical result — real message id, streamed deltas, a schema-constrained tool
// request, and derived usage — and that the credential never appears in any captured
// output. The credential value is used only as an opaque needle for the leak scan and is
// never printed.
package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// anthropicCredentialEnv is the variable name AS IT APPEARS in .env.local (the
// misspelling is the file's, sourced verbatim; never argv/log/evidence/commit).
const anthropicCredentialEnv = "ANTROPHIC_API_KEY"

func liveModelTwo() string {
	if m := os.Getenv("PALAI_LIVE_MODEL_TWO"); m != "" {
		return m
	}
	return "claude-haiku-4-5"
}

// TestLiveProviderTwoTextStreamToolSchema is CASE=text-stream-tool-schema for the
// SECOND provider: one streamed Anthropic message that forces a schema-constrained tool
// call, exercising streaming, tool requests, strict-schema arguments, and usage against
// the REAL Anthropic API in a single call.
func TestLiveProviderTwoTextStreamToolSchema(t *testing.T) {
	secret := os.Getenv(anthropicCredentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", anthropicCredentialEnv)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-two": providertwo.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-two": anthropicCredentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_two_1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModelTwo(),
		Messages: []modelbroker.Message{
			{Role: "user", Content: "Use the add tool to compute the sum of 7 and 5."},
		},
		Tools: []modelbroker.ToolSchema{{
			Name:        "palai.conformance.math.add",
			Description: "Add two integers and return their sum.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"a": map[string]any{"type": "integer"},
					"b": map[string]any{"type": "integer"},
				},
				"required":             []any{"a", "b"},
				"additionalProperties": false,
			},
			Strict: true,
		}},
		ForceToolCall: true,
		Deadline:      time.Now().Add(60 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 4000},
		Secret:        modelbroker.SecretRef("provider-two"),
	}

	var streamed bytes.Buffer
	res, err := broker.Route(context.Background(), "provider-two", req, func(d modelbroker.Delta) {
		streamed.WriteString(d.Text)
		if d.ToolCall != nil {
			streamed.WriteString(d.ToolCall.Name)
			streamed.WriteString(d.ToolCall.ArgumentsFragment)
		}
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if res.Error != nil {
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("live result is not canonical: %v", err)
	}

	if !strings.HasPrefix(res.ProviderRequestID, "msg") {
		t.Errorf("provider request id %q is not a real Anthropic message id", res.ProviderRequestID)
	}
	if len(res.Deltas) == 0 {
		t.Error("no streamed deltas were captured")
	}
	if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 || res.Usage.TotalTokens <= 0 {
		t.Errorf("usage is not populated: %+v", res.Usage)
	}
	if res.Attempts != 1 {
		t.Errorf("attempts = %d, want exactly 1 (no hidden provider retry)", res.Attempts)
	}

	if len(res.ToolCalls) == 0 {
		t.Fatal("the forced tool call produced no tool request")
	}
	call := res.ToolCalls[0]
	if call.Name != "palai.conformance.math.add" {
		t.Errorf("tool name = %q, want palai.conformance.math.add", call.Name)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
		t.Fatalf("tool arguments are not valid JSON: %v", err)
	}
	if _, ok := args["a"]; !ok {
		t.Errorf("tool arguments %v omit the schema-required key a", args)
	}
	if _, ok := args["b"]; !ok {
		t.Errorf("tool arguments %v omit the schema-required key b", args)
	}

	// Leak scan by construction: the credential value must not appear in any captured
	// surface. The comparison is opaque; the value is never printed.
	resultJSON, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	surfaces := map[string][]byte{
		"streamed deltas":  streamed.Bytes(),
		"canonical result": resultJSON,
	}
	for name, captured := range surfaces {
		if bytes.Contains(captured, []byte(secret)) {
			t.Fatalf("%s contains the credential value", name) // never echo the value
		}
	}

	// Safe evidence only: an id prefix, token counts, the finish reason, the tool.
	t.Logf("live PASS provider=provider-two message_id=%s… model=%s finish=%s usage(input=%d output=%d total=%d) tool=%s",
		safePrefix(res.ProviderRequestID), res.Model, res.FinishReason,
		res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, call.Name)
}
