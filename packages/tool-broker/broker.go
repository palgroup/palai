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
	"sort"
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
	Name string
	// Description is the fixed platform text the model reads when the tool is advertised (E12 T1).
	// It is authored by the platform, never tenant-supplied — a tenant description bound to approval
	// is T2's job (§28.2 untrusted-claim discipline). Empty is allowed (a bare conformance tool).
	Description string
	// Type is the Anthropic-defined tool type (e.g. "text_editor_20250728"), empty for an ordinary
	// custom tool. A typed tool's SHAPE LIVES IN THE MODEL: an Anthropic provider is sent {type, name}
	// and no schema, because the schema the tool was trained against is not ours to restate.
	//
	// ‼️ A TYPED TOOL MAY ALSO CARRY InputSchema, AND THAT COMBINATION IS THE PORTABLE ONE. The sentence
	// here used to say a typed tool "leaves InputSchema and Description empty, and the adapter refuses
	// the combination" — which made every typed tool ANTHROPIC-ONLY, and the editor is the tool an agent
	// changes code with. Measured 2026-08-06 on a live OpenAI-routed stack: `palai up` grants the
	// canonical baseline (which contains str_replace_based_edit_tool) to every project AND routes the
	// deployment to provider-one, so the two halves of one bring-up were mutually exclusive and every
	// run died at dispatch — "route this agent to an Anthropic model or drop the tool from its set", on
	// a stack whose own bring-up had chosen both.
	//
	// One tool, two ADVERTISEMENTS: Anthropic takes the type and ignores the schema (the trained shape
	// is the better one there); every other provider advertises the schema as an ordinary function. The
	// EXEC is identical, because the tool's job never depended on the wire shape. A typed tool with NO
	// schema is still Anthropic-only, and the OpenAI adapter still refuses it loudly rather than
	// dropping it — a model with no editor and no sign anything is missing is the worse failure.
	Type         string
	InputSchema  map[string]any
	OutputSchema map[string]any
	ReplayClass  ReplayClass
	// ParallelSafe marks a tool that may run CONCURRENTLY with other parallel-safe tools in the same
	// turn. It is a SEPARATE FIELD FROM ReplayClass on purpose, because that one answers a different
	// question — what happens on replay — and the two disagree in both directions: `show_media` is
	// ClassPure and still writes an artifact, while the file tool is ClassReversible (revertible via
	// snapshot) and must never run beside another write to the same tree. Conflating them would fan
	// out writes.
	//
	// THE ZERO VALUE IS SERIAL, which is the property that matters when a tool is added by someone who
	// never read this comment.
	ParallelSafe bool
	Invoke       func(args map[string]any) (map[string]any, error)
	Exec         func(ctx context.Context, env ExecEnv, args map[string]any) (map[string]any, error)
	// ExternalKeyed marks a tool that sends a real EXTERNAL idempotency key for its side effect (the
	// remote_http invoke's Idempotency-Key = tool_call_id, E12 T4). Its durable pre-write records that key
	// (external_idempotency_key = tool_call_id) so a reconcile can correlate; a built-in with no external
	// key records none (honest: the column is empty unless a real external key exists).
	ExternalKeyed bool
	// Unretained marks a tool whose OUTPUT MAY NOT BE PERSISTED (E21 T5/T7). The dispatcher commits a
	// redaction marker to the tool ledger in its place; the model still receives the real result, because
	// the restriction is on STORING the bytes and not on using them.
	//
	// It exists because the alternative is worse. Every tool result is committed before it is delivered
	// (spec §26.7, and that ordering is load-bearing), so the ledger is where a result lands by default —
	// and `assistant.search.context`'s own terms are "You must not store or copy any of the data retrieved
	// from this API" (§3.5 M5). Without a declared property the orchestrator would need to know the NAME of
	// a Slack tool, which is precisely the coupling the broker exists to prevent.
	//
	// CONSEQUENCE, and it is deliberate: the committed row can no longer replay this call's result across a
	// process kill. That is the honest behaviour for a search whose authority (the event's action_token) did
	// not survive the restart either — a resumed run must ask again rather than be served a copy that should
	// not exist.
	Unretained bool
	// RequiresApproval marks a tool NO CALL OF WHICH RUNS UNTIL A HUMAN DECIDES (E23 T1, spec §22.4).
	// The dispatcher parks the run on it: the ledger row goes to `approval_pending`, an approvals row
	// binds the decision to this call's request hash, and Execute is not reached until an approve
	// advances the row to `ready`.
	//
	// It is DECLARED ON THE TOOL, not per call, and the reason is ReplayClass's own fork one file over
	// ("the class lives on the tool, not per-call DB config") plus the operator's existing ceremony: an
	// MCP tool is already published one at a time, so the gate is a second question beside a decision a
	// human is already making. The honest ceiling rides with it — "approve the transition to Done but not
	// to In Progress" cannot be expressed here, and per-call policy is deferred to the before_tool hook's
	// HookOutcome, which first needs a fail-open/fail-closed answer for a downed hook worker.
	//
	// A built-in declares it in Go and stores NOTHING; a registry/MCP tool carries it on its
	// tool_revisions row (000044 R3), resolved through RequiresApprovalResolved.
	RequiresApproval bool
	// ApprovalLabel is THE ONE HUMAN SENTENCE ON THE APPROVAL SCREEN, and it is the operator's, written at
	// registration (000044 R3). Empty renders as the literal `(no operator label)` rather than as nothing:
	// a blank line reads like "there is no more to say", and the honest reading is "nobody wrote one".
	//
	// It exists as its own column because the two fields a vendor RECOMMENDS for display are the two the
	// same vendor says not to trust: MCP 2025-11-25 documents `title` and `description` as human-readable
	// display text and, on the same page, "clients MUST consider tool annotations to be untrusted unless
	// they come from trusted servers" (§3.5 P3/P4). Reusing tool_revisions.description would put an MCP
	// SERVER's own sentence on the screen a human reads before authorizing that server's side effect.
	ApprovalLabel string
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
	// Unretained is the executed tool's declared no-persist property, copied onto the outcome so the
	// dispatcher can honour it without resolving the tool a second time (Tool.Unretained).
	Unretained bool
}

// LookupFunc resolves a tool absent from the static conformance set by name, scoped by the per-attempt
// ExecEnv (its Scope carries tenant + RunID, so resolution is tenant-scoped). It is how a DB-backed
// registry tool (E12) reaches the broker WITHOUT ever entering the global tool map — no cross-tenant
// leak, no second dispatch loop. Returns (tool, true, nil) on a hit, (_, false, nil) on a clean miss.
type LookupFunc func(ctx context.Context, env ExecEnv, name string) (Tool, bool, error)

// Broker holds the explicit conformance tool set and the fenced tool-call rows.
type Broker struct {
	mu     sync.Mutex
	tools  map[string]Tool
	rows   map[contracts.ToolCallID]*row
	lookup LookupFunc
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

// SetLookup injects the per-tenant registry lookup the broker falls back to on a static-set miss (E12).
// It is set once at composition; a nil lookup keeps the broker's static-only behaviour unchanged.
func (b *Broker) SetLookup(fn LookupFunc) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lookup = fn
}

// Names returns the static conformance tool names (sorted), so a caller can enumerate the built-in set
// without a lock dance. Registry tools resolved through SetLookup are per-tenant and never listed here.
func (b *Broker) Names() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	names := make([]string, 0, len(b.tools))
	for name := range b.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Discoverable reports whether a tool name is in the explicit conformance set.
func (b *Broker) Discoverable(name string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.tools[name]
	return ok
}

// Schema returns a copy of a registered tool's advertised schema (name, description, input schema),
// so the model dispatcher can offer the effective set to the provider without reaching into the
// broker's map. A name outside the set returns ok=false — the dispatcher then never advertises a
// tool the broker cannot execute (spec §28.5, §26.7).
func (b *Broker) Schema(name string) (Tool, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	tool, ok := b.tools[name]
	return tool, ok
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

// SchemaResolved returns a tool's advertised schema, falling back to the injected per-tenant registry
// lookup for a tool absent from the static set (E12 T5) — the twin of ReplayClassResolved. It is what lets
// the model dispatcher advertise a REGISTERED tool (an MCP/remote tool) it can execute but that is not in
// the static broker map; without it a registry tool is resolvable-but-never-advertised, so the model never
// spontaneously calls it. A clean miss or no lookup returns ok=false — the dispatcher then offers nothing.
func (b *Broker) SchemaResolved(ctx context.Context, env ExecEnv, name string) (Tool, bool, error) {
	b.mu.Lock()
	tool, ok := b.tools[name]
	lookup := b.lookup
	b.mu.Unlock()
	if ok {
		return tool, true, nil
	}
	if lookup == nil {
		return Tool{}, false, nil
	}
	return lookup(ctx, env, name)
}

// ReplayClassResolved reports a tool's declared kill-recovery class, falling back to the injected registry
// lookup for a tool absent from the static set (E12) — so a registered tool's DECLARED class (e.g.
// irreversible) drives the pre-write-marker decision at dispatch, NOT the ClassPure static-miss default.
// A clean lookup miss or no lookup yields ClassPure (the caller rejects an unknown tool at Execute).
func (b *Broker) ReplayClassResolved(ctx context.Context, env ExecEnv, name string) (ReplayClass, error) {
	b.mu.Lock()
	tool, ok := b.tools[name]
	lookup := b.lookup
	b.mu.Unlock()
	if ok {
		return tool.replayClass(), nil
	}
	if lookup == nil {
		return ClassPure, nil
	}
	resolved, found, err := lookup(ctx, env, name)
	if err != nil {
		return ClassPure, fmt.Errorf("registry lookup %s: %w", name, err)
	}
	if !found {
		return ClassPure, nil
	}
	return resolved.replayClass(), nil
}

// ExternalKeyedResolved reports whether a tool sends a real EXTERNAL idempotency key for its side effect
// (e.g. the remote_http Idempotency-Key = tool_call_id, E12 T4), so a side-effecting tool's durable
// pre-write records that key for reconcile correlation while a built-in records none. It mirrors
// ReplayClassResolved's static-then-registry-lookup resolution; a clean miss or no lookup is false.
func (b *Broker) ExternalKeyedResolved(ctx context.Context, env ExecEnv, name string) (bool, error) {
	b.mu.Lock()
	tool, ok := b.tools[name]
	lookup := b.lookup
	b.mu.Unlock()
	if ok {
		return tool.ExternalKeyed, nil
	}
	if lookup == nil {
		return false, nil
	}
	resolved, found, err := lookup(ctx, env, name)
	if err != nil {
		return false, fmt.Errorf("registry lookup %s: %w", name, err)
	}
	if !found {
		return false, nil
	}
	return resolved.ExternalKeyed, nil
}

// RequiresApprovalResolved reports whether a tool is gated on a human decision, together with the
// operator's label for the approval screen, falling back to the injected registry lookup for a tool
// absent from the static set. It mirrors ReplayClassResolved's static-then-registry resolution; a clean
// miss or no lookup is (false, "").
//
// THE GATE AND THE LABEL COME BACK FROM ONE LOOKUP ON PURPOSE. The identity a human authorizes has to be
// the identity that executes, so resolving "is this gated" here and "what is it called" somewhere else
// would be two answers that can disagree — and the disagreement would be invisible, because the screen
// would still render. One call, one row, one answer.
func (b *Broker) RequiresApprovalResolved(ctx context.Context, env ExecEnv, name string) (bool, string, error) {
	b.mu.Lock()
	tool, ok := b.tools[name]
	lookup := b.lookup
	b.mu.Unlock()
	if ok {
		return tool.RequiresApproval, tool.ApprovalLabel, nil
	}
	if lookup == nil {
		return false, "", nil
	}
	resolved, found, err := lookup(ctx, env, name)
	if err != nil {
		return false, "", fmt.Errorf("registry lookup %s: %w", name, err)
	}
	if !found {
		return false, "", nil
	}
	return resolved.RequiresApproval, resolved.ApprovalLabel, nil
}

// NeedsPreWrite reports whether a class must be durably marked 'executing' BEFORE it runs so a
// kill-after-execute is detectable as uncertain (spec §26.7): every class whose re-execution is unsafe
// or needs reconciliation — irreversible, reversible, interactive. Pure re-runs freely and idempotent
// resends settle one object, so neither needs the marker.
//
// ANY GUARD ABOUT "WHICH TOOLS CAN WEDGE A RUN" KEYS ON THIS FUNCTION, NEVER ON A TOOL NAME, and that
// is a correction paid for once already. The 2026-08-01 tool-error wedge was reproduced first through
// the FILE tool and had been recorded three days earlier, in an operations doc, as a fact about the
// SHELL tool. They are different classes — file is ClassReversible, shell is ClassIrreversible — and
// they wedge for the identical reason: both answer true here, so both leave a durable `executing` row
// that survives a failed attempt, and the next attempt's ledger consult drives it to `uncertain` and
// then to `manual_resolution`, which nothing resolves. A guard written against "the shell tool" would
// have missed the file tool, and a guard written against "the file tool" would miss the next one. The
// property is the PRE-WRITE, and this is where it is decided.
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
	// Resolution, replay, validation, fence, and the ready→leased→executing transition run under the lock;
	// the row is left `executing` before the lock is released so a later same-id call sees it. The tool's
	// invoke — which for a remote_http tool WAITS on the network (a 202 poll) — runs WITHOUT the lock (MF3),
	// so a slow async tool no longer serializes every other run's tool dispatch; only the bookkeeping is
	// mutually excluded. A completed row still replays cached.
	// ponytail: two concurrent Executes for the SAME tool_call_id are a non-case (one dispatch per call;
	// cross-attempt dedup is the durable ledger's job) — the second sees `executing` and fails its lease.
	b.mu.Lock()
	tool, ok := b.tools[name]
	if !ok {
		// Static-set miss: fall back to the injected per-tenant registry lookup (E12). A hit runs the
		// resolved tool through the SAME fence/ledger/replay machinery below; it never enters b.tools, so
		// resolution stays tenant-scoped. A clean miss (or no lookup) is still ErrUnknownTool.
		//
		// A CLEAN MISS IS AN ANSWER; A FAILED LOOKUP IS NOT (answer.go). The model naming a tool that does
		// not exist is the model being wrong and is told so; the registry lookup itself erroring is a
		// database talking, and that keeps aborting the attempt.
		if b.lookup == nil {
			b.mu.Unlock()
			return Outcome{}, Answerf(AnswerUnknownTool, "%w: %s", ErrUnknownTool, name)
		}
		resolved, found, err := b.lookup(ctx, env, name)
		if err != nil {
			b.mu.Unlock()
			return Outcome{}, fmt.Errorf("registry lookup %s: %w", name, err)
		}
		if !found {
			b.mu.Unlock()
			return Outcome{}, Answerf(AnswerUnknownTool, "%w: %s", ErrUnknownTool, name)
		}
		tool = resolved
	}

	// Idempotent replay: a completed row is authoritative and never re-runs.
	if r, ok := b.rows[callID]; ok && r.state == statemachines.ToolCallCompleted {
		out := Outcome{Result: r.result, State: r.state, Hash: r.hash, ReplayClass: tool.replayClass(),
			Cached: true, Unretained: tool.Unretained}
		b.mu.Unlock()
		return out, nil
	}

	// Strict validation happens before the row or fence is touched, so a rejected
	// argument set produces no side effect and no wasted fence.
	//
	// AND THAT LAST CLAUSE IS EXACTLY WHY AN INPUT REJECTION IS AN ANSWER (answer.go): the tool did not
	// run, so nothing is ambiguous, and "your arguments were wrong" is the single most correctable thing
	// a model can be told. The OUTPUT rejection further down is deliberately NOT an answer — by then the
	// effect has already happened.
	if err := validate(tool.InputSchema, args); err != nil {
		b.mu.Unlock()
		return Outcome{}, Answerf(AnswerInvalidArguments, "%w: input: %v", ErrInvalidArguments, err)
	}

	r := b.rows[callID]
	if r == nil {
		r = &row{state: statemachines.ToolCallReady}
		b.rows[callID] = r
	}
	if err := statemachines.AcceptFence(r.fence, fence); err != nil {
		b.mu.Unlock()
		return Outcome{}, err
	}
	r.fence = fence
	r.hash = requestHash(name, args)

	state, _, err := statemachines.Apply(r.state, statemachines.ToolCallCmdLease, statemachines.ToolCallTable)
	if err != nil {
		b.mu.Unlock()
		return Outcome{}, err
	}
	if state, _, err = statemachines.Apply(state, statemachines.ToolCallCmdExecute, statemachines.ToolCallTable); err != nil {
		b.mu.Unlock()
		return Outcome{}, err
	}
	r.state = state // mark executing in the map, then release the lock across the (possibly network-bound) invoke
	b.mu.Unlock()

	// env is a value copy, so stamping the per-call identity here never touches the caller's template.
	// A remote_http tool reads these for its invoke Idempotency-Key + durable operation row (E12 T4); an
	// mcp tool reads CallID to correlate its advisory progress to this dispatched call (E12 T5).
	env.CallID = callID
	env.Fence = fence
	result, invokeErr := tool.invoke(ctx, env, args)

	b.mu.Lock()
	defer b.mu.Unlock()
	if invokeErr != nil {
		r.state, _, _ = statemachines.Apply(r.state, statemachines.ToolCallCmdFail, statemachines.ToolCallTable)
		// The class and the no-persist property ride the FAILED outcome too. They are properties of the
		// TOOL, not of the call going well, and the dispatcher needs both to commit an answer error onto
		// the same ledger row a success would have taken — an outcome that dropped them would have the
		// row claim `pure` for a reversible tool and would write down the bytes of an unretained one.
		return Outcome{State: r.state, Hash: r.hash, ReplayClass: tool.replayClass(), Unretained: tool.Unretained},
			fmt.Errorf("tool %s: %w", name, invokeErr)
	}
	// AN OUTPUT-SCHEMA FAILURE IS NOT AN ANSWER, and the asymmetry with the input check above is the
	// point: the tool RAN. Whatever it does to the world it has already done, and the honest state of a
	// side effect whose receipt we cannot read is uncertain — which is precisely the machine that already
	// exists for it. Handing the model "that tool's output was malformed" would close a row nobody has
	// reconciled.
	if err := validate(tool.OutputSchema, result); err != nil {
		r.state, _, _ = statemachines.Apply(r.state, statemachines.ToolCallCmdFail, statemachines.ToolCallTable)
		return Outcome{State: r.state, Hash: r.hash, ReplayClass: tool.replayClass(), Unretained: tool.Unretained},
			fmt.Errorf("%w: output: %v", ErrInvalidArguments, err)
	}
	final, _, err := statemachines.Apply(r.state, statemachines.ToolCallCmdComplete, statemachines.ToolCallTable)
	if err != nil {
		return Outcome{}, err
	}
	r.state = final
	r.result = result
	return Outcome{Result: result, State: r.state, Usage: contracts.Usage{ToolCalls: 1}, Hash: r.hash,
		ReplayClass: tool.replayClass(), Unretained: tool.Unretained}, nil
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
