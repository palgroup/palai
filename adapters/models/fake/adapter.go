// Package fake is the deterministic model adapter behind the conformance and
// security suites. It converts a scripted provider exchange into a canonical
// modelbroker.Result — text deltas, tool requests, usage, cancellation, and
// sanitized errors — with no network and no provider SDK, so the canonical
// conversions are asserted the same way the live adapter's are, byte for byte.
package fake

import (
	"context"
	"fmt"
	"sync"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// Script is the deterministic provider exchange the adapter replays.
type Script struct {
	ProviderRequestID string
	Model             string
	TextDeltas        []string
	ToolCalls         []modelbroker.ToolCall
	Output            string // defaults to the joined text deltas when empty
	Usage             contracts.Usage
	Err               *modelbroker.SanitizedError
}

// IdempotencyLedger makes the fake provider idempotent by request key: the first call
// for a key produces the scripted result and counts one effect; a repeat of the same key
// replays that stored result and streams nothing new, counting no additional effect. It
// lets a fault test prove that a reclaimed attempt re-routing the same request after a
// crash settles exactly one provider effect (spec §35.3 idempotent effect, §53.4 single
// retry owner) — the local, no-spend counterpart of a real provider's Idempotency-Key.
type IdempotencyLedger struct {
	mu      sync.Mutex
	keys    []string
	effects int
	stored  map[string]modelbroker.Result
}

// NewIdempotencyLedger returns an empty ledger.
func NewIdempotencyLedger() *IdempotencyLedger {
	return &IdempotencyLedger{stored: map[string]modelbroker.Result{}}
}

// Keys returns every idempotency key the ledger was asked to serve, in call order —
// repeats included, so a test can assert a reclaimed attempt presented the same key.
func (l *IdempotencyLedger) Keys() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.keys...)
}

// Effects returns the number of distinct provider effects: one per first-seen key.
func (l *IdempotencyLedger) Effects() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.effects
}

func (l *IdempotencyLedger) lookup(key string) (modelbroker.Result, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.keys = append(l.keys, key)
	res, ok := l.stored[key]
	return res, ok
}

func (l *IdempotencyLedger) record(key string, res modelbroker.Result) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.stored[key] = res
	l.effects++
}

// Adapter replays one Script as a canonical model call. When Idempotency is set the
// adapter dedups by Request.IdempotencyKey; when nil it replays the script on every call.
type Adapter struct {
	Script      Script
	Idempotency *IdempotencyLedger
}

// Execute streams the scripted deltas and returns the canonical result. It honors
// context cancellation at every increment (each is a safe boundary), so a canceled
// call yields context.Canceled rather than a completed result. The redeemed secret
// is accepted but never used or echoed — the discipline every adapter follows.
func (a Adapter) Execute(ctx context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	if err := ctx.Err(); err != nil {
		return modelbroker.Result{}, err
	}

	// Advertising parity (plan §109): the model may only call a tool it was offered. When the
	// request advertises a tool set, a scripted tool call to a name outside it is a provider fault
	// — the fake never fabricates a call to a tool it was not given. No advertised tools ⇒ inert,
	// so a request that offers none replays the script bit-for-bit as before.
	if len(req.Tools) > 0 {
		offered := make(map[string]struct{}, len(req.Tools))
		for _, t := range req.Tools {
			offered[t.Name] = struct{}{}
		}
		for _, call := range a.Script.ToolCalls {
			if _, ok := offered[call.Name]; !ok {
				return modelbroker.Result{}, fmt.Errorf("provider_error: model called tool %q outside the advertised set", call.Name)
			}
		}
	}

	// Idempotent replay: a repeated key returns the stored result and streams nothing,
	// so no second effect is counted.
	if a.Idempotency != nil && req.IdempotencyKey != "" {
		if stored, ok := a.Idempotency.lookup(req.IdempotencyKey); ok {
			return stored, nil
		}
	}

	var deltas []modelbroker.Delta
	output := a.Script.Output
	for _, text := range a.Script.TextDeltas {
		if err := ctx.Err(); err != nil {
			return modelbroker.Result{}, err
		}
		delta := modelbroker.Delta{Text: text}
		deltas = append(deltas, delta)
		if onDelta != nil {
			onDelta(delta)
		}
		if a.Script.Output == "" {
			output += text
		}
	}
	for i, call := range a.Script.ToolCalls {
		if err := ctx.Err(); err != nil {
			return modelbroker.Result{}, err
		}
		delta := modelbroker.Delta{ToolCall: &modelbroker.ToolCallDelta{
			Index:             i,
			ID:                call.ID,
			Name:              call.Name,
			ArgumentsFragment: call.Arguments,
		}}
		deltas = append(deltas, delta)
		if onDelta != nil {
			onDelta(delta)
		}
	}

	res := modelbroker.Result{
		ModelRequestID:    req.ModelRequestID,
		ProviderRequestID: a.Script.ProviderRequestID,
		Model:             a.Script.Model,
		Output:            output,
		ToolCalls:         a.Script.ToolCalls,
		Deltas:            deltas,
		Usage:             a.Script.Usage,
		Attempts:          1,
	}
	switch {
	case a.Script.Err != nil:
		res.Error = a.Script.Err
		res.FinishReason = "error"
	case len(a.Script.ToolCalls) > 0:
		res.FinishReason = "tool_calls"
	default:
		res.FinishReason = "stop"
	}
	if a.Idempotency != nil && req.IdempotencyKey != "" {
		a.Idempotency.record(req.IdempotencyKey, res)
	}
	return res, nil
}
