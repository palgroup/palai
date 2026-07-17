// Package fake is the deterministic model adapter behind the conformance and
// security suites. It converts a scripted provider exchange into a canonical
// modelbroker.Result — text deltas, tool requests, usage, cancellation, and
// sanitized errors — with no network and no provider SDK, so the canonical
// conversions are asserted the same way the live adapter's are, byte for byte.
package fake

import (
	"context"

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

// Adapter replays one Script as a canonical model call.
type Adapter struct {
	Script Script
}

// Execute streams the scripted deltas and returns the canonical result. It honors
// context cancellation at every increment (each is a safe boundary), so a canceled
// call yields context.Canceled rather than a completed result. The redeemed secret
// is accepted but never used or echoed — the discipline every adapter follows.
func (a Adapter) Execute(ctx context.Context, req modelbroker.Request, _ string, onDelta func(modelbroker.Delta)) (modelbroker.Result, error) {
	if err := ctx.Err(); err != nil {
		return modelbroker.Result{}, err
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
	return res, nil
}
