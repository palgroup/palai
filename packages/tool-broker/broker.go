// Package toolbroker executes pure, in-process conformance tools behind fenced
// tool-call rows (spec §26.7). A tool runs only if it is in the explicit
// conformance set, its arguments pass a strict schema, and its tool_call_id has
// not already completed — a completed row replays its cached result without
// re-executing. Each real execution advances the canonical ToolCallTable
// ready→leased→executing→completed and emits one usage event.
package toolbroker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/palgroup/palai/packages/contracts"
	statemachines "github.com/palgroup/palai/packages/state-machines"
)

var (
	// ErrUnknownTool is returned for a tool outside the explicit conformance set.
	ErrUnknownTool = errors.New("unknown_tool")
	// ErrInvalidArguments is returned when input or output fails its strict schema.
	ErrInvalidArguments = errors.New("invalid_arguments")
)

// ReplayClass is a tool operation's kill-recovery class (spec §26.6). It is DECLARED at registration
// (fork 1: the class lives on the tool, not per-call DB config) and copied onto the ledger row at
// execute time, so a kill-after-execute row is classified without re-deriving it. An unset class
// defaults to ClassPure — the safe, re-runnable-cached default.
type ReplayClass string

const (
	// ClassPure: deterministic, no external side effect — re-run freely, result is "replayed"-labelled
	// but semantically single (TOL-001).
	ClassPure ReplayClass = "pure"
	// ClassIdempotent: a stable destination key makes a resend settle ONE external object (TOL-002).
	ClassIdempotent ReplayClass = "idempotent"
	// ClassReversible: reconcile against the destination first, then compensate/retry per policy (TOL-004).
	ClassReversible ReplayClass = "reversible"
	// ClassIrreversible: a kill after execute enters `uncertain` and NEVER auto-replays; a human resolves
	// it (TOL-003).
	ClassIrreversible ReplayClass = "irreversible"
	// ClassInteractive: no client/approval → no silent replay.
	ClassInteractive ReplayClass = "interactive"
)

// Tool is a broker-fenced tool. A pure conformance tool sets Invoke (deterministic, no side
// effects). A workspace-touching tool (file/shell, spec §28.7-28.8) sets Exec instead: the broker
// prefers Exec, handing it the per-attempt sandbox context so the effect confines to the workspace
// while still riding the same fenced-row, idempotent-replay, schema, and usage machinery. Exactly
// one of Invoke/Exec is set. ReplayClass declares the operation's kill-recovery class (spec §26.6);
// empty means ClassPure.
type Tool struct {
	Name         string
	InputSchema  map[string]any
	OutputSchema map[string]any
	ReplayClass  ReplayClass
	Invoke       func(args map[string]any) (map[string]any, error)
	Exec         func(ctx context.Context, env ExecEnv, args map[string]any) (map[string]any, error)
}

// replayClass reports the tool's declared class, defaulting an unset one to ClassPure.
func (t Tool) replayClass() ReplayClass {
	if t.ReplayClass == "" {
		return ClassPure
	}
	return t.ReplayClass
}

// Outcome is the result of one Execute. Cached reports whether it replayed a
// completed row (in which case the tool did not run and no new usage was emitted). ReplayClass is the
// executed tool's declared kill-recovery class, copied onto the ledger row (spec §26.6).
type Outcome struct {
	Result      map[string]any
	State       statemachines.ToolCallState
	Usage       contracts.Usage
	Hash        string
	ReplayClass ReplayClass
	Cached      bool
}

// Broker holds the explicit conformance tool set and the fenced tool-call rows.
type Broker struct {
	mu    sync.Mutex
	tools map[string]Tool
	rows  map[contracts.ToolCallID]*row
}

type row struct {
	state  statemachines.ToolCallState
	fence  uint64
	hash   string
	result map[string]any
}

// New builds a broker exposing exactly the given tools.
func New(tools ...Tool) *Broker {
	set := make(map[string]Tool, len(tools))
	for _, t := range tools {
		set[t.Name] = t
	}
	return &Broker{tools: set, rows: map[contracts.ToolCallID]*row{}}
}

// Discoverable reports whether a tool name is in the explicit conformance set.
func (b *Broker) Discoverable(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.tools[name]
	return ok
}

// ReplayClassOf reports a tool's declared kill-recovery class BEFORE it executes (spec §26.6), so the
// dispatcher can decide whether a side-effecting tool needs a durable pre-execute marker. An unknown
// tool yields ClassPure (the caller rejects it separately at Execute); a registered tool with no
// declared class defaults ClassPure.
func (b *Broker) ReplayClassOf(name string) ReplayClass {
	b.mu.Lock()
	defer b.mu.Unlock()
	tool, ok := b.tools[name]
	if !ok {
		return ClassPure
	}
	return tool.replayClass()
}

// NeedsPreWrite reports whether a class must be durably marked 'executing' BEFORE it runs so a
// kill-after-execute is detectable as uncertain (spec §26.7): every class whose re-execution is unsafe
// or needs reconciliation — irreversible, reversible, interactive. Pure re-runs freely and idempotent
// resends settle one object, so neither needs the marker.
func NeedsPreWrite(class ReplayClass) bool {
	switch class {
	case ClassIrreversible, ClassReversible, ClassInteractive:
		return true
	default:
		return false
	}
}

// BlocksReplayAfterKill reports whether an in-flight (executing) row of this class, found after a kill,
// must enter `uncertain` rather than silently re-run (spec §26.7): irreversible/interactive never
// auto-replay, and reversible must reconcile-then-compensate first. Pure/idempotent fall through to a
// safe re-execute.
func BlocksReplayAfterKill(class ReplayClass) bool {
	return NeedsPreWrite(class)
}

// Execute runs a conformance tool behind its fenced row. A completed tool_call_id
// replays its cached result unchanged and does not re-execute. A fresh call
// validates its arguments strictly before any side effect, advances the row along
// the canonical table under a strictly increasing fence, invokes the tool,
// validates the output, and caches the completed result with one usage event.
func (b *Broker) Execute(ctx context.Context, callID contracts.ToolCallID, name string, args map[string]any, fence uint64, env ExecEnv) (Outcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	tool, ok := b.tools[name]
	if !ok {
		return Outcome{}, fmt.Errorf("%w: %s", ErrUnknownTool, name)
	}

	// Idempotent replay: a completed row is authoritative and never re-runs.
	if r, ok := b.rows[callID]; ok && r.state == statemachines.ToolCallCompleted {
		return Outcome{Result: r.result, State: r.state, Hash: r.hash, ReplayClass: tool.replayClass(), Cached: true}, nil
	}

	// Strict validation happens before the row or fence is touched, so a rejected
	// argument set produces no side effect and no wasted fence.
	if err := validate(tool.InputSchema, args); err != nil {
		return Outcome{}, fmt.Errorf("%w: input: %v", ErrInvalidArguments, err)
	}

	r := b.rows[callID]
	if r == nil {
		r = &row{state: statemachines.ToolCallReady}
		b.rows[callID] = r
	}
	if err := statemachines.AcceptFence(r.fence, fence); err != nil {
		return Outcome{}, err
	}
	r.fence = fence
	r.hash = requestHash(name, args)

	state, _, err := statemachines.Apply(r.state, statemachines.ToolCallCmdLease, statemachines.ToolCallTable)
	if err != nil {
		return Outcome{}, err
	}
	if state, _, err = statemachines.Apply(state, statemachines.ToolCallCmdExecute, statemachines.ToolCallTable); err != nil {
		return Outcome{}, err
	}

	result, err := tool.invoke(ctx, env, args)
	if err != nil {
		r.state, _, _ = statemachines.Apply(state, statemachines.ToolCallCmdFail, statemachines.ToolCallTable)
		return Outcome{State: r.state, Hash: r.hash}, fmt.Errorf("tool %s: %w", name, err)
	}
	if err := validate(tool.OutputSchema, result); err != nil {
		r.state, _, _ = statemachines.Apply(state, statemachines.ToolCallCmdFail, statemachines.ToolCallTable)
		return Outcome{State: r.state, Hash: r.hash}, fmt.Errorf("%w: output: %v", ErrInvalidArguments, err)
	}

	if state, _, err = statemachines.Apply(state, statemachines.ToolCallCmdComplete, statemachines.ToolCallTable); err != nil {
		return Outcome{}, err
	}
	r.state = state
	r.result = result
	return Outcome{Result: result, State: r.state, Usage: contracts.Usage{ToolCalls: 1}, Hash: r.hash, ReplayClass: tool.replayClass()}, nil
}

// invoke runs the tool through whichever surface it defines: a workspace-touching Exec receives the
// per-attempt sandbox context, a pure conformance Invoke does not. A tool with neither is a
// registration bug caught here rather than as a nil-call panic.
func (t Tool) invoke(ctx context.Context, env ExecEnv, args map[string]any) (map[string]any, error) {
	switch {
	case t.Exec != nil:
		return t.Exec(ctx, env, args)
	case t.Invoke != nil:
		return t.Invoke(args)
	default:
		return nil, fmt.Errorf("tool %s has no invoke surface", t.Name)
	}
}

// RequestHash is the exported canonical hash of a tool call (name, args), so the dispatcher can record
// the SAME digest on the durable pre-write that the broker records at completion — one definition of the
// content identity a duplicate tool_call_id is recognised by (spec §25.9, §26.6).
func RequestHash(name string, args map[string]any) string { return requestHash(name, args) }

// requestHash is the canonical hash of a tool call. json.Marshal sorts map keys,
// so the digest is stable for equal (name, args) pairs (spec §25.9 same request-id
// carries the same hash).
func requestHash(name string, args map[string]any) string {
	canonical, _ := json.Marshal([]any{name, args})
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
