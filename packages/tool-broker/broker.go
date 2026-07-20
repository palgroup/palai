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

// Tool is a broker-fenced tool. A pure conformance tool sets Invoke (deterministic, no side
// effects). A workspace-touching tool (file/shell, spec §28.7-28.8) sets Exec instead: the broker
// prefers Exec, handing it the per-attempt sandbox context so the effect confines to the workspace
// while still riding the same fenced-row, idempotent-replay, schema, and usage machinery. Exactly
// one of Invoke/Exec is set.
type Tool struct {
	Name         string
	InputSchema  map[string]any
	OutputSchema map[string]any
	Invoke       func(args map[string]any) (map[string]any, error)
	Exec         func(ctx context.Context, env ExecEnv, args map[string]any) (map[string]any, error)
}

// ExecEnv is the per-attempt sandbox context a workspace-touching tool receives (spec §28.7-28.8):
// the resolved workspace root every path confines to, and a ShellRunner for argv execution. A pure
// conformance tool ignores it; a zero ExecEnv (no workspace bound) makes a workspace tool fail
// cleanly rather than escape.
type ExecEnv struct {
	WorkspaceRoot string
	ReadOnly      bool
	Shell         ShellRunner
}

// ShellRunner runs one argv command inside the sandbox and returns its captured, bounded result.
// The concrete implementation (an OCI-driver-backed sandbox) lives outside this dependency-light
// package; the seam keeps the broker free of sandbox mechanics.
type ShellRunner interface {
	Run(ctx context.Context, cmd ShellCommand) (ShellResult, error)
}

// ShellCommand is one sandboxed execution request: the argv (never a shell string — the caller opts
// into a shell explicitly), and whether the workspace is writable for this call.
type ShellCommand struct {
	Argv      []string
	ReadOnly  bool
	Shell     bool
	StdinData []byte
}

// ShellResult is the captured outcome of a sandboxed command: bounded, already-redacted output, the
// exit code / signal, and the resource usage the sandbox recorded.
type ShellResult struct {
	ExitCode   int
	Signal     string
	Stdout     string
	Stderr     string
	Truncated  bool
	TimedOut   bool
	OOMKilled  bool
	DurationMS int64
}

// Outcome is the result of one Execute. Cached reports whether it replayed a
// completed row (in which case the tool did not run and no new usage was emitted).
type Outcome struct {
	Result map[string]any
	State  statemachines.ToolCallState
	Usage  contracts.Usage
	Hash   string
	Cached bool
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
		return Outcome{Result: r.result, State: r.state, Hash: r.hash, Cached: true}, nil
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
	return Outcome{Result: result, State: r.state, Usage: contracts.Usage{ToolCalls: 1}, Hash: r.hash}, nil
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

// requestHash is the canonical hash of a tool call. json.Marshal sorts map keys,
// so the digest is stable for equal (name, args) pairs (spec §25.9 same request-id
// carries the same hash).
func requestHash(name string, args map[string]any) string {
	canonical, _ := json.Marshal([]any{name, args})
	sum := sha256.Sum256(canonical)
	return "sha256:" + hex.EncodeToString(sum[:])
}
