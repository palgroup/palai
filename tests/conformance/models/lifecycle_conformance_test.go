package models_test

// Runtime conformance — MOD-009 cancel, MOD-006 partial-stream, MOD-007 idempotent replay
// (E16 T6). These pin the runtime lifecycle behaviors the broker+adapters already honor,
// asserted across the adapter families where the behavior is provider-relevant. Cancel is
// provider-agnostic (fake per-delta) plus proven over a REAL pending HTTP read for both wire
// families; truncation is a wire behavior (the fake has no stream to cut), proven on both wire
// families; idempotent replay is the fake ledger — the local, no-spend counterpart of a real
// provider's Idempotency-Key, which the wire adapters forward.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	fake "github.com/palgroup/palai/adapters/models/fake"
	providerone "github.com/palgroup/palai/adapters/models/provider_one"
	providertwo "github.com/palgroup/palai/adapters/models/provider_two"
	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// blockingSSEServer writes prefix, flushes it, then holds the connection open until the client's
// request context is canceled — so a mid-stream cancel interrupts a REAL pending read rather than
// racing an already-buffered transcript. It is the deterministic stand-in for a provider that is
// still streaming when the caller cancels.
func blockingSSEServer(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(prefix))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done() // hold the stream open until the caller cancels
	}))
	t.Cleanup(srv.Close)
	return srv
}

// oneStreamCancelPrefix / twoStreamCancelPrefix are the first delta each wire family sends before
// the caller cancels mid-stream.
const oneStreamCancelPrefix = `data: {"id":"chatcmpl-CANCEL1","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"role":"assistant","content":"Par"},"finish_reason":null}]}

`

const twoStreamCancelPrefix = `event: message_start
data: {"type":"message_start","message":{"id":"msg_CANCEL2","model":"claude-canon","usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Par"}}

`

// TestAdaptersHonorMidStreamCancel is MOD-009: a cancel while the stream is still open is honored —
// the call surfaces context.Canceled (never a fabricated completed result) and the partial deltas
// the caller already saw are retained. Proven provider-agnostically with the fake (a per-delta
// cancel checkpoint) and over a REAL pending HTTP read for both wire families.
func TestAdaptersHonorMidStreamCancel(t *testing.T) {
	cases := []struct {
		name    string
		adapter func(t *testing.T) modelbroker.ModelAdapter
	}{
		{"fake", func(t *testing.T) modelbroker.ModelAdapter {
			return fake.Adapter{Script: fake.Script{
				ProviderRequestID: "prov-fake-cancel", Model: "fake-1",
				TextDeltas: []string{"Par", "tial", "-more"},
				Usage:      contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8},
			}}
		}},
		{"provider-one", func(t *testing.T) modelbroker.ModelAdapter {
			return providerone.Adapter{BaseURL: blockingSSEServer(t, oneStreamCancelPrefix).URL}
		}},
		{"provider-two", func(t *testing.T) modelbroker.ModelAdapter {
			return providertwo.Adapter{BaseURL: blockingSSEServer(t, twoStreamCancelPrefix).URL}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			broker := modelbroker.New(modelbroker.Config{
				Adapters: map[string]modelbroker.ModelAdapter{"p": c.adapter(t)},
				Secrets:  modelbroker.StaticResolver{"p": sentinelSecret},
			})
			req := baseRequest()
			req.Deadline = time.Now().Add(30 * time.Second)
			req.Secret = "p"

			ctx, cancel := context.WithCancel(context.Background())
			var partial []modelbroker.Delta
			res, err := broker.Route(ctx, "p", req, func(d modelbroker.Delta) {
				partial = append(partial, d)
				cancel() // cancel the moment the first delta is seen — mid-stream
			})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("mid-stream cancel: got err=%v res=%+v, want context.Canceled", err, res)
			}
			if len(partial) == 0 {
				t.Error("no partial delta was delivered before cancel — the caller must retain what it saw")
			}
		})
	}
}

// oneStreamTruncated / twoStreamTruncated cut off after one content delta: EOF before any terminal
// marker ([DONE] / message_stop), any finish reason, or (for provider-one) a usage chunk.
const oneStreamTruncated = `data: {"id":"chatcmpl-TRUNC1","object":"chat.completion.chunk","model":"gpt-4o-mini-canned","choices":[{"index":0,"delta":{"role":"assistant","content":"Hel"},"finish_reason":null}]}

`

const twoStreamTruncated = `event: message_start
data: {"type":"message_start","message":{"id":"msg_TRUNC2","model":"claude-canon","usage":{"input_tokens":5,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}

`

// TestWireAdaptersSurfaceTruncatedStreamAsVisiblePartial is MOD-006: a stream that cuts off mid-way
// surfaces the partial deterministically — the partial output and provider id are kept, there is
// exactly ONE attempt (no hidden/seamless retry), and the finish reason stays EMPTY so the cutoff
// is VISIBLE rather than masked as a clean completion. Truncation is a wire behavior, proven on both
// wire families; the fake has no stream to cut.
func TestWireAdaptersSurfaceTruncatedStreamAsVisiblePartial(t *testing.T) {
	cases := []struct {
		name    string
		adapter func(t *testing.T) modelbroker.ModelAdapter
	}{
		{"provider-one", func(t *testing.T) modelbroker.ModelAdapter {
			return providerone.Adapter{BaseURL: sseServer(t, oneStreamTruncated).URL}
		}},
		{"provider-two", func(t *testing.T) modelbroker.ModelAdapter {
			return providertwo.Adapter{BaseURL: sseServer(t, twoStreamTruncated).URL}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := routeText(t, c.adapter(t))
			if err := res.Validate(); err != nil {
				t.Fatalf("a truncated result is still canonical: %v", err)
			}
			if res.Output == "" {
				t.Error("the partial output must stay visible after a truncation")
			}
			if res.ProviderRequestID == "" {
				t.Error("the provider id seen before truncation must be kept")
			}
			if res.FinishReason != "" {
				t.Errorf("finish reason = %q, want empty — a cutoff must not be masked as a clean finish", res.FinishReason)
			}
			if res.Attempts != 1 {
				t.Errorf("attempts = %d, want exactly 1 (no hidden/seamless retry on a truncation)", res.Attempts)
			}
		})
	}
}

// TestFakeIdempotentReplayServesStoredEffectWithoutReExecuting is MOD-007: re-routing the SAME
// idempotency key returns the STORED result and streams nothing new — no blind tool-producing
// replay — so exactly one provider effect is settled even when a crash window re-opens the call.
// A DIFFERENT key is a distinct effect. Provider-agnostic: the fake ledger is the local, no-spend
// counterpart of a real provider's Idempotency-Key, which provider-one/two forward on the wire.
func TestFakeIdempotentReplayServesStoredEffectWithoutReExecuting(t *testing.T) {
	ledger := fake.NewIdempotencyLedger()
	adapter := fake.Adapter{
		Idempotency: ledger,
		Script: fake.Script{
			ProviderRequestID: "prov-fake-idem", Model: "fake-1",
			ToolCalls: []modelbroker.ToolCall{{ID: "call_1", Name: "palai.conformance.math.add", Arguments: `{"a":7,"b":5}`}},
			Usage:     contracts.Usage{InputTokens: 9, OutputTokens: 5, TotalTokens: 14, ToolCalls: 1},
		},
	}
	broker := modelbroker.New(modelbroker.Config{
		Adapters: map[string]modelbroker.ModelAdapter{"p": adapter},
		Secrets:  modelbroker.StaticResolver{"p": sentinelSecret},
	})
	req := canonicalToolRequest()
	req.Secret = "p"
	req.IdempotencyKey = "run-1/mreq-1"

	var firstDeltas, replayDeltas int
	first, err := broker.Route(context.Background(), "p", req, func(modelbroker.Delta) { firstDeltas++ })
	if err != nil {
		t.Fatalf("first route: %v", err)
	}
	replay, err := broker.Route(context.Background(), "p", req, func(modelbroker.Delta) { replayDeltas++ })
	if err != nil {
		t.Fatalf("replay route: %v", err)
	}

	if ledger.Effects() != 1 {
		t.Errorf("effects = %d, want exactly 1 (the replay settled no second provider effect)", ledger.Effects())
	}
	if firstDeltas == 0 {
		t.Error("the first call must stream its deltas")
	}
	if replayDeltas != 0 {
		t.Errorf("replay streamed %d deltas, want 0 (no blind tool-producing replay)", replayDeltas)
	}
	// The replayed result IS the stored effect — same tool call id, same provider id.
	if len(replay.ToolCalls) != 1 || len(first.ToolCalls) != 1 || replay.ToolCalls[0].ID != first.ToolCalls[0].ID {
		t.Errorf("replay tool calls = %+v, want the stored effect %+v", replay.ToolCalls, first.ToolCalls)
	}
	if replay.ProviderRequestID != first.ProviderRequestID {
		t.Errorf("replay provider id = %q, want the stored %q", replay.ProviderRequestID, first.ProviderRequestID)
	}

	// A DIFFERENT key is a distinct provider effect.
	req.IdempotencyKey = "run-1/mreq-2"
	if _, err := broker.Route(context.Background(), "p", req, nil); err != nil {
		t.Fatalf("distinct-key route: %v", err)
	}
	if ledger.Effects() != 2 {
		t.Errorf("effects = %d after a distinct key, want 2", ledger.Effects())
	}
}
