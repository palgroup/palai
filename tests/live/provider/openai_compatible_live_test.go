//go:build live

// Package provider_test's OpenAI-compatible leg is the MOD-002 capability-probe smoke.
// It runs only under the `live` build tag, in
// `make test-live-provider PROVIDER=openai-compatible CASE=capability-probe`, which loads
// the real OpenAI credential from .env.local. Two legs: (1) LIVE — the ACTIVE probe against
// the REAL OpenAI endpoint (OpenAI-compatible by definition) observes streaming +
// tool-calling + strict-JSON; (2) a LOCAL fake "private" endpoint that lacks tool-calling
// makes a run that hard-requires a tool call REJECT PRE-run — the real completion is never
// sent. Honest ceiling: a real private/self-hosted server (vLLM/Ollama) probe+run is a §6
// operator leg; the local fake models the reject-on-unsupported-feature contract.
package provider_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestLiveOpenAICompatibleCapabilityProbe proves the active probe end to end: the real
// OpenAI endpoint is observed as fully capable, and a run demanding tool-calling against a
// local endpoint that lacks it is rejected before any provider call.
func TestLiveOpenAICompatibleCapabilityProbe(t *testing.T) {
	secret := os.Getenv(credentialEnv)
	if secret == "" {
		t.Fatalf("%s is unset; the live tier loads it from .env.local at runtime", credentialEnv)
	}

	// Leg 1 (LIVE): probe the REAL OpenAI endpoint. It is OpenAI-compatible by definition,
	// so the active probe must observe all three capabilities.
	rec, err := openaicompatible.NewProber().Probe(context.Background(), providerone.DefaultBaseURL, secret, liveModel())
	if err != nil {
		t.Fatalf("live probe of the real OpenAI endpoint: %v", err)
	}
	if !rec.Streaming || !rec.ToolCalls || !rec.StructuredJSON {
		t.Fatalf("real OpenAI endpoint probed as %+v, want all capabilities observed", rec)
	}
	if rec.LastValidated.IsZero() {
		t.Error("live probe did not stamp last_validated")
	}
	t.Logf("live PASS provider=openai-compatible probe(real OpenAI): streaming=%v tool_calls=%v strict_json=%v validated=%s",
		rec.Streaming, rec.ToolCalls, rec.StructuredJSON, rec.LastValidated.UTC().Format(time.RFC3339))

	// Leg 2 (LOCAL fake private endpoint): an endpoint that 400s the tools probe lacks
	// tool-calling, so a run that hard-requires a tool call is rejected PRE-run — the real
	// completion never reaches the endpoint.
	var realCompletions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"tools"`) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"tools not supported","type":"invalid_request_error"}}`)
			return
		}
		if strings.Contains(string(body), "palai_conformance_math_add") {
			realCompletions.Add(1)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: openaicompatible.NewProber()}
	req := modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_live_compat_reject"),
		Model:          "private-model",
		Messages:       []modelbroker.Message{{Role: "user", Content: "add 7 and 5"}},
		Tools: []modelbroker.ToolSchema{{
			Name:       "palai.conformance.math.add",
			Parameters: map[string]any{"type": "object", "required": []any{"a", "b"}},
			Strict:     true,
		}},
		ForceToolCall: true,
		Deadline:      time.Now().Add(30 * time.Second),
		Reservation:   modelbroker.Reservation{MaxTotalTokens: 100},
	}
	// A dummy credential — this endpoint is local and never a real provider.
	_, err = adapter.Execute(context.Background(), req, "sk-local-fake-unused", nil)
	if !errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
		t.Fatalf("execute error = %v, want ErrCapabilityUnsupported (pre-run reject)", err)
	}
	if got := realCompletions.Load(); got != 0 {
		t.Fatalf("the run reached the endpoint %d time(s); a rejected run must never call the provider", got)
	}
	t.Logf("live PASS provider=openai-compatible probe(local private endpoint): tool-calling absent -> run rejected PRE-run, real completions=%d", realCompletions.Load())
}
