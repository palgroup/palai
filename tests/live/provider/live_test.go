//go:build live

// Package provider_test is the protected live-provider smoke. It runs only under
// the `live` build tag, only in `make test-live-provider`, which loads the real
// credential from .env.local into the environment at runtime. It proves the
// provider-one adapter converts a real streamed chat completion into a canonical
// result — real provider request id, streamed deltas, a schema-constrained tool
// request, and usage — and that the credential never appears in any captured
// output. The credential value is used only as an opaque needle for the leak scan
// and is never printed.
package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

const credentialEnv = "OPENAI_API_KEY"

func liveModel() string {
	if m := os.Getenv("PALAI_LIVE_MODEL"); m != "" {
		return m
	}
	return "gpt-4o-mini"
}

// TestLiveProviderOneTextStreamToolSchema is CASE=text-stream-tool-schema: one
// streamed chat completion that forces a schema-constrained tool call, so it
// exercises streaming, tool requests, strict-schema arguments, and usage against
// the real provider in a single call.
func TestLiveProviderOneTextStreamToolSchema(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"provider-one": providerone.Adapter{}},
		Secrets:  modelbroker.EnvResolver{"provider-one": credentialEnv},
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          liveModel(),
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
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 2000},
		Secret:        modelbroker.SecretRef("provider-one"),
	}

	var streamed bytes.Buffer
	res, err := broker.Route(context.Background(), "provider-one", req, func(d modelbroker.Delta) {
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
		// Report the status class and stable code only — never the raw body or key.
		t.Fatalf("provider returned a sanitized error: code=%s status=%d", res.Error.Code, res.Error.Status)
	}
	if err := res.Validate(); err != nil {
		t.Fatalf("live result is not canonical: %v", err)
	}

	if !strings.HasPrefix(res.ProviderRequestID, "chatcmpl") {
		t.Errorf("provider request id %q is not a real chat completion id", res.ProviderRequestID)
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

	// Leak scan by construction: the credential value must not appear in any
	// captured surface. The comparison is opaque; the value is never printed.
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
	t.Logf("live PASS provider_request_id=%s… model=%s finish=%s usage(input=%d output=%d total=%d) tool=%s",
		safePrefix(res.ProviderRequestID), res.Model, res.FinishReason,
		res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.TotalTokens, call.Name)
}

// safePrefix returns a short, non-sensitive prefix of a provider id for evidence.
func safePrefix(id string) string {
	if len(id) > 16 {
		return id[:16]
	}
	return id
}
