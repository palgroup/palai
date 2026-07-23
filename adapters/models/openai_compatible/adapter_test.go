package openaicompatible_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

const secret = "sk-compat-SENTINEL-000"

// cannedToolStream is a minimal OpenAI ChatCompletions SSE that produces one tool call
// — enough to prove the embedded provider-one conversion runs after admission passes.
const cannedToolStream = `data: {"id":"chatcmpl-COMPAT1","object":"chat.completion.chunk","model":"compat-model","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"palai_conformance_math_add","arguments":"{\"a\":7,\"b\":5}"}}]},"finish_reason":null}]}

data: {"id":"chatcmpl-COMPAT1","object":"chat.completion.chunk","model":"compat-model","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}

data: {"id":"chatcmpl-COMPAT1","object":"chat.completion.chunk","model":"compat-model","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":5,"total_tokens":14}}

data: [DONE]

`

func toolRequest() modelbroker.Request {
	return modelbroker.Request{
		ModelRequestID: contracts.ModelRequestID("mreq_compat_1"),
		Model:          "compat-model",
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
}

// TestProbeDetectsFullEndpoint drives the active probe against an endpoint that accepts
// every feature (2xx) and asserts the observed record reports all three capabilities and
// carries a validation timestamp.
func TestProbeDetectsFullEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	rec, err := openaicompatible.NewProber().Probe(context.Background(), srv.URL, secret, "compat-model")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !rec.Streaming || !rec.ToolCalls || !rec.StructuredJSON {
		t.Errorf("full endpoint record = %+v, want all capabilities observed", rec)
	}
	if rec.LastValidated.IsZero() {
		t.Error("probe did not stamp last_validated")
	}
}

// TestAdmitsAndRunsWhenCapable preloads a permissive capability record (isolating the
// wire path from the network probe) and proves admission passes, then the run delegates
// to the embedded provider-one conversion and returns a canonical tool result.
func TestAdmitsAndRunsWhenCapable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedToolStream)
	}))
	defer srv.Close()

	prober := openaicompatible.NewProber()
	prober.Preload(srv.URL, openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: true, StructuredJSON: true})
	adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: prober}

	res, err := adapter.Execute(context.Background(), toolRequest(), secret, nil)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	res.ModelRequestID = toolRequest().ModelRequestID
	if err := res.Validate(); err != nil {
		t.Fatalf("result is not canonical: %v", err)
	}
	if len(res.ToolCalls) != 1 || res.ToolCalls[0].Name != "palai.conformance.math.add" {
		t.Fatalf("tool calls = %+v, want the reused conversion to restore the canonical name", res.ToolCalls)
	}
	if res.ProviderRequestID != "chatcmpl-COMPAT1" {
		t.Errorf("provider id = %q, want the streamed value from the reused conversion", res.ProviderRequestID)
	}
}

// TestRejectsToolRunOnEndpointWithoutToolCalling is MOD-002: an endpoint that does not
// support tool-calling (it 400s the tools probe) causes a run that hard-requires a tool
// call to be REJECTED PRE-run — the real completion is never sent to the endpoint.
func TestRejectsToolRunOnEndpointWithoutToolCalling(t *testing.T) {
	var realCompletions atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		text := string(body)
		if strings.Contains(text, `"tools"`) {
			// This limited endpoint does not support tool-calling: reject the probe.
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"tools not supported","type":"invalid_request_error"}}`)
			return
		}
		if strings.Contains(text, "palai_conformance_math_add") {
			realCompletions.Add(1) // a real run reached the endpoint — must never happen
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, cannedToolStream)
	}))
	defer srv.Close()

	adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: openaicompatible.NewProber()}
	_, err := adapter.Execute(context.Background(), toolRequest(), secret, nil)
	if !errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
		t.Fatalf("execute error = %v, want ErrCapabilityUnsupported (pre-run reject)", err)
	}
	if got := realCompletions.Load(); got != 0 {
		t.Errorf("the run reached the endpoint %d time(s); a rejected run must never call the provider", got)
	}
}

// TestAdmissionGuardsEachHardRequirement covers the three admission branches with
// preloaded records (no network): a run streams unconditionally, hard-requires
// tool-calling when it forces tools, and hard-requires strict-JSON when it constrains
// output to a schema. Each missing capability rejects; a fully-capable record admits.
func TestAdmissionGuardsEachHardRequirement(t *testing.T) {
	schemaReq := func() modelbroker.Request {
		r := modelbroker.Request{
			ModelRequestID: contracts.ModelRequestID("mreq_compat_schema"),
			Model:          "compat-model",
			Messages:       []modelbroker.Message{{Role: "user", Content: "hi"}},
			OutputSchema:   &modelbroker.OutputSchema{Name: "x", Schema: map[string]any{"type": "object"}, Strict: true},
			Deadline:       time.Now().Add(30 * time.Second),
			Reservation:    modelbroker.Reservation{MaxTotalTokens: 100},
		}
		return r
	}
	cases := []struct {
		name   string
		record openaicompatible.CapabilityRecord
		req    modelbroker.Request
		reject bool
	}{
		{"no-streaming", openaicompatible.CapabilityRecord{Streaming: false, ToolCalls: true, StructuredJSON: true}, toolRequest(), true},
		{"tools-required-missing", openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: false, StructuredJSON: true}, toolRequest(), true},
		{"schema-required-missing", openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: true, StructuredJSON: false}, schemaReq(), true},
		{"schema-required-present", openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: true, StructuredJSON: true}, schemaReq(), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base := "http://never-dialed.invalid/v1/chat/completions"
			prober := openaicompatible.NewProber()
			prober.Preload(base, c.record)
			adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: base}, Prober: prober}
			_, err := adapter.Execute(context.Background(), c.req, secret, nil)
			if c.reject {
				if !errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
					t.Fatalf("error = %v, want ErrCapabilityUnsupported", err)
				}
				return
			}
			// Admitted: it proceeds to the (bogus) endpoint and fails at the transport, NOT at admission.
			if errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
				t.Fatalf("a capable record was rejected at admission: %v", err)
			}
		})
	}
}

// TestStaleRecordIsReprobed proves a record older than the TTL is not trusted: a second
// probe past the TTL re-observes the endpoint rather than serving the stale record.
func TestStaleRecordIsReprobed(t *testing.T) {
	var probes atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		probes.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	clock := time.Unix(1_700_000_000, 0)
	prober := openaicompatible.NewProber()
	prober.TTL = time.Minute
	prober.Now = func() time.Time { return clock }

	if _, err := prober.Probe(context.Background(), srv.URL, secret, "m"); err != nil {
		t.Fatalf("first probe: %v", err)
	}
	firstProbes := probes.Load()
	if firstProbes == 0 {
		t.Fatal("first probe made no observation")
	}
	// Within TTL: cached, no new observation.
	if _, err := prober.Probe(context.Background(), srv.URL, secret, "m"); err != nil {
		t.Fatalf("cached probe: %v", err)
	}
	if probes.Load() != firstProbes {
		t.Errorf("a fresh record was re-probed within the TTL")
	}
	// Past TTL: re-observed.
	clock = clock.Add(2 * time.Minute)
	if _, err := prober.Probe(context.Background(), srv.URL, secret, "m"); err != nil {
		t.Fatalf("stale probe: %v", err)
	}
	if probes.Load() <= firstProbes {
		t.Errorf("a stale record was served without re-probing (probes stayed at %d)", firstProbes)
	}
}

// TestTransient429IsNotCachedAsUnsupported proves the operational fix: a transient 429
// on the probe returns a probe ERROR (not a capability answer) and does NOT cache an
// all-false record — so a rate-limit blip does not become a full-TTL outage. When the
// endpoint recovers, the next probe re-observes it as fully capable.
func TestTransient429IsNotCachedAsUnsupported(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTooManyRequests)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
		_, _ = io.WriteString(w, "{}")
	}))
	defer srv.Close()

	prober := openaicompatible.NewProber()
	_, err := prober.Probe(context.Background(), srv.URL, secret, "m")
	if err == nil {
		t.Fatal("a 429 probe returned no error; a transient blip must not be treated as a capability answer")
	}
	if errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
		t.Fatalf("429 surfaced as a capability rejection, not a probe error: %v", err)
	}
	// Endpoint recovers; the next probe must re-observe (proving no all-false was cached).
	status.Store(http.StatusOK)
	rec, err := prober.Probe(context.Background(), srv.URL, secret, "m")
	if err != nil {
		t.Fatalf("recovered probe: %v", err)
	}
	if !rec.Streaming || !rec.ToolCalls || !rec.StructuredJSON {
		t.Fatalf("recovered record = %+v, want all-true (a poisoned all-false cache would have been served instead)", rec)
	}
}

// TestRejectErrorRedactsCredentialInBaseURL proves a credential carried in the endpoint
// URL (a gateway with a query-param token) is not echoed into the rejection error.
func TestRejectErrorRedactsCredentialInBaseURL(t *testing.T) {
	base := "https://gw.example/v1/chat/completions?api-key=SECRETTOKEN123"
	prober := openaicompatible.NewProber()
	prober.Preload(base, openaicompatible.CapabilityRecord{Streaming: false}) // streaming-missing rejects before any dial
	adapter := openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: base}, Prober: prober}
	_, err := adapter.Execute(context.Background(), toolRequest(), secret, nil)
	if !errors.Is(err, openaicompatible.ErrCapabilityUnsupported) {
		t.Fatalf("err = %v, want a capability rejection", err)
	}
	if strings.Contains(err.Error(), "SECRETTOKEN123") {
		t.Fatalf("rejection error leaked the credential in the base URL: %v", err)
	}
}

// assert cannedToolStream parses as a well-formed forced-tool exchange guard: the args
// fragment must be valid JSON so the reused conversion yields parseable arguments.
func TestCannedStreamArgsAreValidJSON(t *testing.T) {
	if !strings.Contains(cannedToolStream, `{\"a\":7,\"b\":5}`) {
		t.Fatal("canned stream lost its tool arguments")
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(`{"a":7,"b":5}`), &args); err != nil {
		t.Fatalf("arguments fixture is not valid JSON: %v", err)
	}
}
