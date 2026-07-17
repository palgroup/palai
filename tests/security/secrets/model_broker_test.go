// Package secrets_test proves the model broker's credential discipline without a
// network or a real credential: a sentinel secret is resolved from a SecretRef
// only inside the executor and never appears in any captured delta, canonical
// Result, persisted request, or broker diagnostic line (plan security rule; spec
// §20 secret handling). The scan is by construction — an opaque byte comparison
// that never prints the sentinel, so a failure reports a leak without echoing it.
package secrets_test

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// sentinelSecret stands in for a live credential. It must never be emitted; the
// test only ever compares against it, never prints it.
const sentinelSecret = "sk-live-SENTINEL-9c3f-must-never-appear-anywhere"

func TestSecretRefIsRedeemedOnlyInExecutorAndNeverLeaks(t *testing.T) {
	var diagnostics bytes.Buffer
	broker := modelbroker.New(modelbroker.Config{
		Adapters:    map[string]modelbroker.ModelAdapter{"provider-one": fake.Adapter{Script: leakScript()}},
		Secrets:     modelbroker.StaticResolver{"provider-one": sentinelSecret},
		Diagnostics: &diagnostics,
	})

	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_secleak1"),
		RouteRevision:  1,
		ModelStepID:    "step-1",
		Model:          "fake-1",
		Messages:       []modelbroker.Message{{Role: "user", Content: "hello"}},
		Deadline:       time.Date(2026, time.July, 16, 12, 0, 5, 0, time.UTC),
		Reservation:    modelbroker.Reservation{MaxTotalTokens: 100},
		Secret:         modelbroker.SecretRef("provider-one"),
	}

	// The SecretRef on the request is a name, not the value.
	if string(req.Secret) == sentinelSecret || bytes.Contains(mustJSON(t, req), []byte(sentinelSecret)) {
		t.Fatal("the request carries the raw credential; SecretRef must be an opaque name")
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

	// Every surface a credential could leak into: the streamed deltas, the
	// canonical result (which becomes events/frames), the request as persisted,
	// and the broker's own diagnostics.
	surfaces := map[string][]byte{
		"streamed deltas":    streamed.Bytes(),
		"canonical result":   mustJSON(t, res),
		"persisted request":  mustJSON(t, req),
		"broker diagnostics": diagnostics.Bytes(),
	}
	for name, captured := range surfaces {
		if bytes.Contains(captured, []byte(sentinelSecret)) {
			t.Fatalf("%s contains the credential value", name) // never print the value itself
		}
	}

	// The diagnostics must still prove the executor ran (a non-secret breadcrumb),
	// so the absence above is real coverage, not an empty capture.
	if diagnostics.Len() == 0 {
		t.Error("executor produced no diagnostics to scan")
	}
	if res.Error == nil {
		t.Error("scenario expected a sanitized provider error on the result")
	}
}

// leakScript drives every canonical surface at once: text deltas, a tool request,
// usage, and a sanitized provider error.
func leakScript() fake.Script {
	return fake.Script{
		ProviderRequestID: "chatcmpl-secleak",
		Model:             "fake-1",
		TextDeltas:        []string{"partial ", "answer"},
		ToolCalls: []modelbroker.ToolCall{{
			ID:        "call_leak",
			Name:      "palai.conformance.math.add",
			Arguments: `{"a":7,"b":5}`,
		}},
		Usage: contracts.Usage{InputTokens: 6, OutputTokens: 3, TotalTokens: 9, ToolCalls: 1},
		Err:   &modelbroker.SanitizedError{Code: "provider_error", Message: "upstream declined", Status: 400},
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return data
}
