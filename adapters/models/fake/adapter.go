// Package fake is the deterministic model adapter behind the conformance and
// security suites. It converts a scripted provider exchange into a canonical
// modelbroker.Result — text deltas, tool requests, usage, cancellation, and
// sanitized errors — with no network and no provider SDK, so the canonical
// conversions are asserted the same way the live adapter's are, byte for byte.
package fake

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/palgroup/palai/packages/contracts"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
)

// Script is the deterministic provider exchange the adapter replays: ONE turn, plus the follow-up
// turns that answer once the model has been handed tool results.
//
// The field tags are the wire form LoadScript reads, and they exist because a script the adapter
// answers with can arrive from OUTSIDE the binary — see LoadScript for what a deployment gets by
// routing one in.
type Script struct {
	ProviderRequestID string                      `json:"provider_request_id,omitempty"`
	Model             string                      `json:"model,omitempty"`
	TextDeltas        []string                    `json:"text_deltas,omitempty"`
	ToolCalls         []modelbroker.ToolCall      `json:"tool_calls,omitempty"`
	Output            string                      `json:"output,omitempty"` // defaults to the joined text deltas when empty
	Usage             contracts.Usage             `json:"usage,omitempty"`
	Err               *modelbroker.SanitizedError `json:"error,omitempty"`

	// Then are the FOLLOW-UP turns, in order, and they are what makes a scripted exchange
	// multi-step: turn one asks for a tool, the engine runs it and calls the model again, and
	// Then[0] is what that second call answers.
	//
	// A script with no Then is single-turn and replays on every call, exactly as it did before this
	// field existed — which is every Script this tree constructs in Go.
	//
	// THE LIST IS FLAT. Only the first turn's Then is ever read; a Then written INSIDE a follow-up
	// turn describes turns nothing replays, so LoadScript — the only way a Then arrives from outside
	// the binary — refuses one rather than dropping it.
	Then []Script `json:"then,omitempty"`
}

// turnFor selects the turn to replay: one turn per contiguous group of tool-result messages already
// in the conversation. A run that has been handed no tool result yet is on turn one (this Script);
// one carrying the results of the first turn's calls is on Then[0], and so on.
//
// IT COUNTS GROUPS, NOT MESSAGES, because a turn may call several tools and the engine answers all
// of them before calling the model again — counting messages would skip a turn per extra call. The
// discriminator is the conversation itself rather than a call counter, so a re-routed attempt
// (spec §53.4) replays the same turn instead of advancing past it.
//
// Past the last turn the last turn repeats. LoadScript refuses a routed script whose last turn calls
// a tool, so a repeat there is an answer, not a loop.
func (s Script) turnFor(msgs []modelbroker.Message) Script {
	if len(s.Then) == 0 {
		return s
	}
	taken, inGroup := 0, false
	for _, m := range msgs {
		if m.Role != "tool" {
			inGroup = false
			continue
		}
		if !inGroup {
			taken++
		}
		inGroup = true
	}
	if taken == 0 {
		return s
	}
	if taken > len(s.Then) {
		taken = len(s.Then)
	}
	return s.Then[taken-1]
}

// LoadScript reads a scripted exchange from a JSON file — the form a DEPLOYMENT routes one in.
//
// IT EXISTS BECAUSE A STACK WITH NO CREDENTIAL COULD PROVE NO TOOL PATH AT ALL. The adapter this
// package ships answers a fixed script with no tool calls, and nothing could point it at another
// one, so every end-to-end proof that a tool ran on a machine needed a provider key — and this tree
// exposes no tools to a real provider, so the key would not have produced a tool call either. A run
// that calls a tool and reads its result had no reachable path on a self-host at all.
//
// A FILE PATH RATHER THAN THE SCRIPT ITSELF, and the reason is measured. The variable travels
// through operator files that REWRITE their values: compose interpolates `${...}` out of `.env`, and
// this tree has already shipped a defect where dotenv expansion ate the `$` segments of a console
// password hash. A JSON document carries `"`, `{` and — in a shell tool's arguments — a plain `$`.
// A path carries none of them, and it is the shape four other variables this binary reads already
// use for a structured value (PALAI_ENROLLMENT_TOKEN_FILE, PALAI_BOOTSTRAP_API_KEY_FILE,
// PALAI_SECRET_MASTER_KEY_FILE, PALAI_GITHUB_APP_PRIVATE_KEY_FILE).
//
// EVERY REFUSAL HERE IS THE POINT. A missing file, a typo'd field, a turn that answers nothing, a
// last turn that calls a tool — each one, silently defaulted, would leave an operator watching a run
// that reaches no tool while believing their script is driving it. That is the exact belief this
// seam exists to make impossible, so a script that cannot be replayed as written is an error, never
// a fallback to the built-in one.
func LoadScript(path string) (Script, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Script{}, fmt.Errorf("read scripted exchange: %w", err)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// A misspelled field is a turn that does something other than what the file says. Unknown fields
	// are refused for the same reason the rest of this function refuses: the operator must find out
	// here, not from a run that answered "" and called nothing.
	dec.DisallowUnknownFields()
	var s Script
	if err := dec.Decode(&s); err != nil {
		return Script{}, fmt.Errorf("parse scripted exchange %s: %w", path, err)
	}
	if dec.More() {
		return Script{}, fmt.Errorf("parse scripted exchange %s: trailing content after the script object", path)
	}
	if err := s.validate(); err != nil {
		return Script{}, fmt.Errorf("scripted exchange %s: %w", path, err)
	}
	return s, nil
}

// validate refuses the three shapes a replay cannot honour as written. It is called by LoadScript
// only: a Script built in Go is code under review, and this is for a document that is not.
func (s Script) validate() error {
	turns := append([]Script{s}, s.Then...)
	for i, turn := range turns {
		if i > 0 && len(turn.Then) > 0 {
			return fmt.Errorf("turn %d nests its own `then`: follow-up turns are a flat list on the first turn, "+
				"and a nested one would be replayed by nothing", i+1)
		}
		if turn.Output == "" && len(turn.TextDeltas) == 0 && len(turn.ToolCalls) == 0 && turn.Err == nil {
			return fmt.Errorf("turn %d answers nothing: it has no output, no text deltas, no tool call and no error, "+
				"so the run it drives would look exactly like the built-in script this file was written to replace", i+1)
		}
	}
	if last := turns[len(turns)-1]; len(last.ToolCalls) > 0 {
		return fmt.Errorf("the last turn (%d of %d) calls a tool: past the last turn the last turn repeats, so this "+
			"script asks for that tool forever and the run never answers — end it with a turn that answers",
			len(turns), len(turns))
	}
	return nil
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

	// The turn this conversation is on. A single-turn script (every one built in Go in this tree)
	// selects itself, so everything below is what it always was.
	script := a.Script.turnFor(req.Messages)

	// Advertising parity (plan §109): the model may only call a tool it was offered. When the
	// request advertises a tool set, a scripted tool call to a name outside it is a provider fault
	// — the fake never fabricates a call to a tool it was not given. No advertised tools ⇒ inert,
	// so a request that offers none replays the script bit-for-bit as before.
	//
	// It judges THIS TURN's calls against THIS request's advertised set, because both change from
	// one model step to the next: a run that offers the shell tool only after a file read would
	// otherwise be failed by a later turn's legitimate call.
	if len(req.Tools) > 0 {
		offered := make(map[string]struct{}, len(req.Tools))
		for _, t := range req.Tools {
			offered[t.Name] = struct{}{}
		}
		for _, call := range script.ToolCalls {
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
	output := script.Output
	for _, text := range script.TextDeltas {
		if err := ctx.Err(); err != nil {
			return modelbroker.Result{}, err
		}
		delta := modelbroker.Delta{Text: text}
		deltas = append(deltas, delta)
		if onDelta != nil {
			onDelta(delta)
		}
		if script.Output == "" {
			output += text
		}
	}
	for i, call := range script.ToolCalls {
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
		ProviderRequestID: script.ProviderRequestID,
		Model:             script.Model,
		Output:            output,
		ToolCalls:         script.ToolCalls,
		Deltas:            deltas,
		Usage:             script.Usage,
		Attempts:          1,
	}
	switch {
	case script.Err != nil:
		res.Error = script.Err
		res.FinishReason = "error"
	case len(script.ToolCalls) > 0:
		res.FinishReason = "tool_calls"
	default:
		res.FinishReason = "stop"
	}
	if a.Idempotency != nil && req.IdempotencyKey != "" {
		a.Idempotency.record(req.IdempotencyKey, res)
	}
	return res, nil
}
