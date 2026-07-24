package models_test

// Runtime conformance — MOD-010 prompt-cache usage (E16 T6). Every provider family folds
// its OWN prompt-cache counters into the ONE canonical Usage shape, so cache usage is
// visible and normalized across providers rather than trapped in a provider-specific
// field. The suite is adapter-PARAMETRIC (the T5 pattern): fake + provider-one (OpenAI) +
// provider-two (Anthropic) + the OpenAI-compatible adapter each drive their own wire
// fixture through the broker and converge on the same two canonical counters.

import (
	"context"
	"testing"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	openaicompatible "github.com/palgroup/palai/adapters/models/openai_compatible"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// cachedUsageStreamOne is a provider-one usage chunk carrying prompt_tokens_details.cached_tokens
// — OpenAI's cache-READ signal. OpenAI reports no explicit cache-WRITE count (it auto-caches), so
// the canonical write counter stays 0 for this family: an honest normalization, not a fabricated
// number.
const cachedUsageStreamOne = `data: {"id":"chatcmpl-CACHED1","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}

data: {"id":"chatcmpl-CACHED1","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[],"usage":{"prompt_tokens":1050,"completion_tokens":4,"total_tokens":1054,"prompt_tokens_details":{"cached_tokens":1024}}}

data: [DONE]

`

// cachedUsageStreamTwo is a provider-two message carrying cache_read_input_tokens AND
// cache_creation_input_tokens — Anthropic reports BOTH a cache-read and a cache-creation (write)
// count, so this family exercises both canonical cache counters. Anthropic reports input/output
// SEPARATELY from the cache counters (they are not folded into input_tokens), so the base usage
// invariant is unaffected.
const cachedUsageStreamTwo = `event: message_start
data: {"type":"message_start","message":{"id":"msg_CACHED2","model":"claude-canon","usage":{"input_tokens":11,"output_tokens":1,"cache_read_input_tokens":1024,"cache_creation_input_tokens":50}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":4}}

event: message_stop
data: {"type":"message_stop"}

`

// routeText runs one adapter through the broker on a plain text request (with the live deadline
// the wire adapters' HTTP needs, and an unbounded reservation so a large cached prompt is not
// budget-rejected), returning the canonical result.
func routeText(t *testing.T, adapter modelbroker.ModelAdapter) modelbroker.Result {
	t.Helper()
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"p": adapter},
		Secrets:  modelbroker.StaticResolver{"p": sentinelSecret},
	})
	req := baseRequest()
	req.Deadline = time.Now().Add(30 * time.Second)
	req.Secret = "p"
	req.Reservation = modelbroker.Reservation{} // unbounded — the cached-prompt cases exceed the base 100-token cap
	res, err := broker.Route(context.Background(), "p", req, nil)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	return res
}

// TestAdaptersFoldProviderCacheCountersIntoCanonicalUsage is MOD-010: each family's native
// prompt-cache counters fold into the SAME two canonical Usage fields. OpenAI contributes a
// cache-read only; Anthropic contributes read + creation; the fake carries whatever a script
// sets — all land in CacheReadTokens / CacheWriteTokens, and the base usage invariant holds.
func TestAdaptersFoldProviderCacheCountersIntoCanonicalUsage(t *testing.T) {
	cases := []struct {
		name           string
		adapter        func(t *testing.T) modelbroker.ModelAdapter
		wantCacheRead  int
		wantCacheWrite int
	}{
		{"fake", func(t *testing.T) modelbroker.ModelAdapter {
			return fake.Adapter{Script: fake.Script{
				ProviderRequestID: "prov-fake-cache",
				Model:             "fake-1",
				TextDeltas:        []string{"hi"},
				Usage:             contracts.Usage{InputTokens: 26, OutputTokens: 4, TotalTokens: 30, CacheReadTokens: 1024, CacheWriteTokens: 50},
			}}
		}, 1024, 50},
		{"provider-one", func(t *testing.T) modelbroker.ModelAdapter {
			return providerone.Adapter{BaseURL: sseServer(t, cachedUsageStreamOne).URL}
		}, 1024, 0},
		{"provider-two", func(t *testing.T) modelbroker.ModelAdapter {
			return providertwo.Adapter{BaseURL: sseServer(t, cachedUsageStreamTwo).URL}
		}, 1024, 50},
		{"openai-compatible", func(t *testing.T) modelbroker.ModelAdapter {
			srv := sseServer(t, cachedUsageStreamOne)
			prober := openaicompatible.NewProber()
			prober.Preload(srv.URL, openaicompatible.CapabilityRecord{Streaming: true, ToolCalls: true, StructuredJSON: true})
			return openaicompatible.Adapter{Adapter: providerone.Adapter{BaseURL: srv.URL}, Prober: prober}
		}, 1024, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := routeText(t, c.adapter(t))
			if err := res.Validate(); err != nil {
				t.Fatalf("result is not canonical: %v", err)
			}
			if res.Usage.CacheReadTokens != c.wantCacheRead {
				t.Errorf("cache read = %d, want %d (provider cache-read folded to canonical Usage)", res.Usage.CacheReadTokens, c.wantCacheRead)
			}
			if res.Usage.CacheWriteTokens != c.wantCacheWrite {
				t.Errorf("cache write = %d, want %d (provider cache-creation folded to canonical Usage)", res.Usage.CacheWriteTokens, c.wantCacheWrite)
			}
			// The cache counters are additive metering; they never break the base usage invariant.
			if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 {
				t.Errorf("usage = %+v, want populated input/output", res.Usage)
			}
			if res.Usage.CacheReadTokens < 0 || res.Usage.CacheWriteTokens < 0 {
				t.Errorf("cache counters must be non-negative: %+v", res.Usage)
			}
		})
	}
}
