// Package execution's orchestrator is the single response kernel: it drives a run
// through the canonical state machine and a live engine channel, committing every
// provider and tool result before it reaches the engine. It writes no second agent
// loop — the engine owns the loop; the orchestrator only correlates requests, commits
// state, and dispatches (spec §24.7, §25.10).
package execution

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/palgroup/palai/apps/control-plane/internal/store"
	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/coordinator"
	modelbroker "github.com/palgroup/palai/packages/model-broker"
	"github.com/palgroup/palai/packages/runner"
	statemachines "github.com/palgroup/palai/packages/state-machines"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

const engineProtocol = "engine.v1"

// dialHandshakeDeadline bounds one attempt's engine dial + engine.ready handshake. It is
// shorter than the 30s worker lease the dispatcher grants (main.go startDispatch) so a
// stuck dial fails the attempt — routed through the existing retry / dead-letter path —
// well before the lease lapses, turning a silent hang into a classified, retryable failure.
const dialHandshakeDeadline = 20 * time.Second

// Orchestrator executes response run attempts through the common kernel.
type Orchestrator struct {
	store  *store.Store
	spine  *coordinator.Store
	dialer EngineDialer
	models *modelbroker.Broker
	tools  *toolbroker.Broker
	route  ModelRoute
	// DialHandshakeDeadline bounds the dial + engine.ready handshake per attempt. Zero uses
	// dialHandshakeDeadline; NewOrchestrator sets the default. Tests shorten it.
	DialHandshakeDeadline time.Duration
}

// NewOrchestrator binds the durable store, the engine dialer, and the model and tool
// brokers into one kernel. The model route defaults to the deterministic fake provider;
// main.go overrides it for a live provider via SetModelRoute.
func NewOrchestrator(st *store.Store, dialer EngineDialer, models *modelbroker.Broker, tools *toolbroker.Broker) *Orchestrator {
	return &Orchestrator{store: st, spine: st.Spine(), dialer: dialer, models: models, tools: tools, route: defaultModelRoute, DialHandshakeDeadline: dialHandshakeDeadline}
}

// SetModelRoute points the kernel at a non-default provider/model/secret selected by the
// composition root (main.go) from the environment. ponytail: a setter, not a model_routes
// lookup — the DB-backed routing is the deferred E-series carve-out.
func (o *Orchestrator) SetModelRoute(r ModelRoute) { o.route = r }

// attemptState is the per-attempt working set threaded through the dispatch handlers.
type attemptState struct {
	attempt        AttemptDescriptor
	tenant         coordinator.Tenant
	sessionID      string
	responseID     string
	ch             EngineChannel
	ledger         *runner.FrameLedger
	seq            int // controller frame sequence (engine ignores it; kept envelope-valid)
	lastInboundSeq int // last accepted engine frame sequence; the intake requires the next to be +1
	output         []contracts.ContentItem
	usage          contracts.Usage
	model          string // the actually-used model from the latest committed model result
}

// ExecuteAttempt drives one run attempt to a terminal outcome. It provisions and
// starts the run through canonical transitions, opens the engine channel, and runs
// the frame-intake loop: every frame is validated and deduped before any dispatch,
// and every provider/tool result is committed before it is delivered to the engine.
func (o *Orchestrator) ExecuteAttempt(ctx context.Context, attempt AttemptDescriptor) error {
	tenant, sessionID, responseID, input, err := o.spine.RunContext(ctx, string(attempt.RunID))
	if err != nil {
		return err
	}

	// Move the run into execution using canonical transitions only. A run already
	// advanced past a step (redelivery) is skipped; a run already terminal is left
	// alone (spec §22.3).
	for _, cmd := range []statemachines.RunCommand{statemachines.RunCmdProvision, statemachines.RunCmdStart} {
		switch _, err := o.spine.ApplyRunTransition(ctx, tenant, string(attempt.RunID), cmd); {
		case errors.Is(err, coordinator.ErrRunTerminal):
			return nil
		case errors.Is(err, statemachines.ErrInvalidState):
			// already applied by an earlier attempt; resume idempotently
		case err != nil:
			return err
		}
	}

	// Bound the dial + engine.ready handshake with an attempt-scoped deadline: a runner that
	// connects but whose handshake wedges (or a gateway with no available runner) must fail
	// the attempt — routed through retry / dead-letter — not hang it silently. The deadline
	// covers only Dial and the ready receive below; the run loop that follows uses the parent
	// ctx, so a long-running response is never cut off at the deadline.
	dialCtx, cancelDial := context.WithTimeout(ctx, o.DialHandshakeDeadline)
	defer cancelDial()

	ch, err := o.dialer.Dial(dialCtx, attempt)
	if err != nil {
		return fmt.Errorf("dial engine: %w", err)
	}
	defer func() { _ = ch.Close() }()

	st := &attemptState{
		attempt: attempt, tenant: tenant, sessionID: sessionID, responseID: responseID,
		ch: ch, ledger: runner.NewFrameLedger(),
	}

	ready, err := ch.Receive(dialCtx)
	if err != nil {
		return fmt.Errorf("receive engine.ready: %w", err)
	}
	if _, err := st.ledger.Admit(ready); err != nil {
		return fmt.Errorf("engine.ready: %w", err)
	}
	if ready.Type != "engine.ready" {
		return fmt.Errorf("first frame type = %q, want engine.ready", ready.Type)
	}
	st.lastInboundSeq = ready.Sequence

	var inputValue any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &inputValue)
	}
	// Carry the session's prior responses as conversation history so a chained response
	// continues the session (spec §9, §22.2). A first response has no prior — messages is
	// omitted and run.start is exactly the LP-0 single-shot shape.
	prior, err := o.spine.SessionHistory(ctx, tenant, sessionID, responseID)
	if err != nil {
		return fmt.Errorf("assemble session history: %w", err)
	}
	runStart := map[string]any{"input": inputValue}
	if messages := historyMessages(prior); len(messages) > 0 {
		runStart["messages"] = messages
	}
	if err := ch.Send(ctx, o.frame(st, "run.start", runStart, string(ready.ID))); err != nil {
		return fmt.Errorf("send run.start: %w", err)
	}

	// Apply any config switch accepted for this session that had no boundary in its own run — an
	// idle-session change_config, or a single-step run — so this run's first model step routes
	// under it (spec §9.3, the cross-run config carry). Runs before the first model.request; a
	// switch aimed at a mid-run boundary is untouched (it is applied by the pump/watcher instead).
	if err := o.applyPendingSessionConfig(ctx, st); err != nil {
		return abortIfTerminal(err)
	}

	for {
		frame, err := ch.Receive(ctx)
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("engine closed the channel before a terminal frame: %w", err)
		}
		if err != nil {
			return fmt.Errorf("receive frame: %w", err)
		}
		if err := validateFrame(frame, attempt); err != nil {
			return err
		}
		// Frame-ID dedup (ENG-002 controller half): a repeat with the same hash is an
		// idempotent retransmit, a repeat with a different hash is a protocol violation.
		duplicate, err := st.ledger.Admit(frame)
		if err != nil {
			return fmt.Errorf("frame ledger: %w", err)
		}
		if duplicate {
			continue
		}
		// Intake sequence monotonicity: after dedup, every accepted engine frame must
		// carry the next sequence. A gap or reorder is a protocol violation that fails the
		// attempt before any dispatch, matching the batch supervisor's index+1 rule. A
		// dropped retransmit above does not advance the sequence — it rides the same
		// ordered stream with the same number.
		if frame.Sequence != st.lastInboundSeq+1 {
			return fmt.Errorf("engine frame %s sequence %d is not %d (non-monotonic)", frame.ID, frame.Sequence, st.lastInboundSeq+1)
		}
		st.lastInboundSeq = frame.Sequence

		switch frame.Type {
		case "model.request":
			continues, err := o.dispatchModel(ctx, st, frame)
			if err != nil {
				return abortIfTerminal(err)
			}
			// After a model result is committed and delivered, this is a safe boundary
			// (spec §25.11). When the run continues to another step, drain any queued/steered
			// messages here so they fold into the NEXT model request — the input boundary
			// (spec §9.2). A final result has no next step, so nothing is delivered.
			if continues {
				switch err := o.pumpCommands(ctx, st); {
				case errors.Is(err, errRunPaused):
					// A pause landed at this boundary: the run is waiting, the attempt ends cleanly
					// and releases its compute, and resume re-opens a fresh attempt (spec §22.3).
					return nil
				case err != nil:
					return abortIfTerminal(err)
				}
			}
		case "tool.request":
			if err := o.dispatchTool(ctx, st, frame); err != nil {
				return abortIfTerminal(err)
			}
		case "output.item":
			st.output = append(st.output, contracts.ContentItem(frame.Data))
		case "run.terminal":
			return o.finalize(ctx, st, frame)
		case "protocol.error":
			return fmt.Errorf("engine protocol error: %v", frame.Data)
		default:
			// progress, warning, heartbeat, run.waiting: nothing to commit or dispatch.
		}
	}
}

// frame builds a controller frame with the attempt identity and a monotonic sequence.
func (o *Orchestrator) frame(st *attemptState, typ string, data map[string]any, replyTo string) contracts.EngineFrame {
	st.seq++
	f := contracts.EngineFrame{
		Protocol:  engineProtocol,
		ID:        newFrameID(),
		Type:      typ,
		Sequence:  st.seq,
		Time:      time.Now().UTC().Format(time.RFC3339),
		RunID:     st.attempt.RunID,
		AttemptID: st.attempt.AttemptID,
		Data:      data,
	}
	if replyTo != "" {
		rt := replyTo
		f.ReplyTo = &rt
	}
	return f
}

// validateFrame enforces the engine envelope and the run/attempt identity before any
// transaction, so a malformed or misrouted frame never reaches a dispatch.
func validateFrame(f contracts.EngineFrame, a AttemptDescriptor) error {
	if f.Protocol != engineProtocol || !f.ID.Valid() || f.Type == "" {
		return fmt.Errorf("frame violates the engine envelope")
	}
	if _, err := time.Parse(time.RFC3339, f.Time); err != nil {
		return fmt.Errorf("frame %s has no valid timestamp", f.ID)
	}
	if f.RunID != "" && f.RunID != a.RunID {
		return fmt.Errorf("frame %s run identity mismatch", f.ID)
	}
	if f.AttemptID != "" && f.AttemptID != a.AttemptID {
		return fmt.Errorf("frame %s attempt identity mismatch", f.ID)
	}
	return nil
}

// abortIfTerminal maps a mid-attempt terminal-run rejection to a clean attempt end. When a
// run is canceled while an attempt is in flight, its next commit is rejected with
// ErrRunTerminal (the commit-after-terminal guard); the attempt then has nothing left to do
// — the run is already terminal — so it ends without error and the durable job settles
// instead of dead-lettering. Any other error still fails the attempt.
func abortIfTerminal(err error) error {
	if errors.Is(err, coordinator.ErrRunTerminal) {
		return nil
	}
	return err
}

func newFrameID() contracts.FrameID {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return contracts.FrameID("frm_" + hex.EncodeToString(raw[:]))
}
