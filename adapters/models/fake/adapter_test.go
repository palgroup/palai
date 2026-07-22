package fake

import (
	"context"
	"testing"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// TestAdapterDedupsByIdempotencyKey proves the idempotent fake settles exactly one effect
// across two calls carrying the same key — the local, no-spend proof that a reclaimed,
// re-routed request does not double-charge the provider (spec §53.4, §35.3).
func TestAdapterDedupsByIdempotencyKey(t *testing.T) {
	ledger := NewIdempotencyLedger()
	adapter := Adapter{
		Script:      Script{ProviderRequestID: "prov_1", Model: "fake", Output: "hi", Usage: contracts.Usage{InputTokens: 5, OutputTokens: 3, TotalTokens: 8}},
		Idempotency: ledger,
	}
	req := modelbroker.Request{ModelRequestID: "mreq_dedup1", IdempotencyKey: "run_dedup1/mreq_dedup1"}

	first, err := adapter.Execute(context.Background(), req, "secret", nil)
	if err != nil {
		t.Fatalf("first Execute error = %v", err)
	}
	second, err := adapter.Execute(context.Background(), req, "secret", nil)
	if err != nil {
		t.Fatalf("second Execute error = %v", err)
	}

	if ledger.Effects() != 1 {
		t.Fatalf("provider effects = %d, want 1 (the repeated key must not re-run)", ledger.Effects())
	}
	if keys := ledger.Keys(); len(keys) != 2 || keys[0] != req.IdempotencyKey || keys[1] != req.IdempotencyKey {
		t.Fatalf("recorded keys = %v, want the same key twice", keys)
	}
	if first.Output != second.Output || second.Output != "hi" {
		t.Fatalf("replayed result = %q, want the stored %q", second.Output, first.Output)
	}
}

// TestAdapterFaultsOnUnadvertisedToolCall proves the fake honors advertising parity (plan §109):
// when the request advertises a tool set, a scripted tool call to a name outside it is a provider
// fault — the fake never fabricates a call to a tool it was not offered. With no advertised tools
// the check is inert and the script replays unchanged (bit-for-bit the pre-advertising behavior).
func TestAdapterFaultsOnUnadvertisedToolCall(t *testing.T) {
	adapter := Adapter{Script: Script{
		ProviderRequestID: "prov_1", Model: "fake",
		ToolCalls: []modelbroker.ToolCall{{ID: "c1", Name: "palai.workspace.shell", Arguments: "{}"}},
	}}

	// Advertised set offers only file; a scripted shell call is outside it → provider fault.
	offered := modelbroker.Request{ModelRequestID: "mreq_adv1", Tools: []modelbroker.ToolSchema{{Name: "palai.workspace.file"}}}
	if _, err := adapter.Execute(context.Background(), offered, "secret", nil); err == nil {
		t.Fatal("advertised only file but scripted a shell call; want a provider fault, got nil")
	}

	// The SAME script with no advertised tools → the check is inert, the script replays unchanged.
	unadvertised := modelbroker.Request{ModelRequestID: "mreq_adv2"}
	if _, err := adapter.Execute(context.Background(), unadvertised, "secret", nil); err != nil {
		t.Fatalf("no advertised tools should replay the script unchanged, got %v", err)
	}

	// A call to the advertised tool passes.
	adapter.Script.ToolCalls[0].Name = "palai.workspace.file"
	if _, err := adapter.Execute(context.Background(), offered, "secret", nil); err != nil {
		t.Fatalf("calling the advertised tool should pass, got %v", err)
	}
}

// TestAdapterWithoutLedgerReplaysEveryCall proves the default fake (no ledger) is
// unchanged: it replays its script on every call, deduping nothing.
func TestAdapterWithoutLedgerReplaysEveryCall(t *testing.T) {
	adapter := Adapter{Script: Script{ProviderRequestID: "prov_1", Model: "fake", Output: "hi"}}
	req := modelbroker.Request{ModelRequestID: "mreq_plain1", IdempotencyKey: "run_plain1/mreq_plain1"}

	for i := 0; i < 2; i++ {
		res, err := adapter.Execute(context.Background(), req, "secret", nil)
		if err != nil || res.Output != "hi" {
			t.Fatalf("Execute #%d = %q, %v, want the scripted output", i, res.Output, err)
		}
	}
}
