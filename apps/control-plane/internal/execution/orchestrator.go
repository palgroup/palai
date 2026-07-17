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

// Orchestrator executes response run attempts through the common kernel.
type Orchestrator struct {
	store  *store.Store
	spine  *coordinator.Store
	dialer EngineDialer
	models *modelbroker.Broker
	tools  *toolbroker.Broker
}

// NewOrchestrator binds the durable store, the engine dialer, and the model and tool
// brokers into one kernel.
func NewOrchestrator(st *store.Store, dialer EngineDialer, models *modelbroker.Broker, tools *toolbroker.Broker) *Orchestrator {
	return &Orchestrator{store: st, spine: st.Spine(), dialer: dialer, models: models, tools: tools}
}

// attemptState is the per-attempt working set threaded through the dispatch handlers.
type attemptState struct {
	attempt    AttemptDescriptor
	tenant     coordinator.Tenant
	sessionID  string
	responseID string
	ch         EngineChannel
	ledger     *runner.FrameLedger
	seq        int // controller frame sequence (engine ignores it; kept envelope-valid)
	output     []contracts.ContentItem
	usage      contracts.Usage
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

	ch, err := o.dialer.Dial(ctx, attempt)
	if err != nil {
		return fmt.Errorf("dial engine: %w", err)
	}
	defer func() { _ = ch.Close() }()

	st := &attemptState{
		attempt: attempt, tenant: tenant, sessionID: sessionID, responseID: responseID,
		ch: ch, ledger: runner.NewFrameLedger(),
	}

	ready, err := ch.Receive(ctx)
	if err != nil {
		return fmt.Errorf("receive engine.ready: %w", err)
	}
	if _, err := st.ledger.Admit(ready); err != nil {
		return fmt.Errorf("engine.ready: %w", err)
	}
	if ready.Type != "engine.ready" {
		return fmt.Errorf("first frame type = %q, want engine.ready", ready.Type)
	}

	var inputValue any
	if len(input) > 0 {
		_ = json.Unmarshal(input, &inputValue)
	}
	if err := ch.Send(ctx, o.frame(st, "run.start", map[string]any{"input": inputValue}, string(ready.ID))); err != nil {
		return fmt.Errorf("send run.start: %w", err)
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

		switch frame.Type {
		case "model.request":
			if err := o.dispatchModel(ctx, st, frame); err != nil {
				return err
			}
		case "tool.request":
			if err := o.dispatchTool(ctx, st, frame); err != nil {
				return err
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

func newFrameID() contracts.FrameID {
	var raw [16]byte
	_, _ = rand.Read(raw[:])
	return contracts.FrameID("frm_" + hex.EncodeToString(raw[:]))
}
