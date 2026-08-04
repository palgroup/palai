package execution

// ADDRESSING A MACHINE, WHICH IS WHAT THE GATEWAY COULD NOT DO (A.3 T7).
//
// Every message the control plane sent a runner before this file rode an ATTEMPT's lease: Dial hands
// back a gatewayChannel and everything travels on it, which works because the attempt is alive while it
// speaks. A background task is the opposite case by definition — it outlives the attempt that started
// it — so the reconciler that sweeps it later holds no lease, and there is nothing to speak on.
//
// The pieces were already here and the wiring was not. Measured before this file (2026-08-04):
//
//	sed -n '1756,1757p' runner_gateway.go   -> `if gc == nil { continue }`  // parked: DISCARDS
//	grep -n "func (g *RunnerGateway) [A-Z]" runner_gateway.go
//	  -> Cordon/Resume/Approve/RevokeRunner (lifecycle), Connected/Waiting (counters), Dial (by ATTEMPT)
//	     ...and nothing that writes to a named machine
//
// So the gateway held the connection (g.sessions) and the identity (pendingRunner.runnerID) and could
// not put a byte on the first addressed by the second. That is what this adds, and it is deliberately
// the SMALLEST thing that closes it: one send, one correlated reply, and the three lifecycle rules.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/palgroup/palai/packages/contracts"
	"github.com/palgroup/palai/packages/runner"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ErrMachineUnreachable is what a caller gets when the named machine has no live session on this
// gateway, or has one this control plane is not allowed to speak on. It is deliberately ONE error for
// both, because a caller's correct response is the same for either: report `lost` — the process may
// still be running and this control plane cannot prove it is ours — and never signal.
var ErrMachineUnreachable = errors.New("no reachable session for this machine")

// machineCall is one answer, routed by bg_id.
type machineCall struct {
	data map[string]any
	err  error
}

// machineCalls is the set of questions this gateway has asked machines and not yet heard back about.
// It is the same shape as the exec registry a lease uses (remote_shell.go execPending) and for the same
// reasons — buffered so delivery never blocks the connection's sole reader, entry removed under the
// lock so each waiter sees exactly one send — but it lives on the GATEWAY rather than on a channel,
// because the thing it correlates has no channel to live on.
type machineCalls struct {
	mu      sync.Mutex
	waiting map[string]chan machineCall
}

func (c *machineCalls) register(id string) (<-chan machineCall, func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, taken := c.waiting[id]; taken {
		return nil, nil, fmt.Errorf("background call %s is already awaiting an answer", id)
	}
	if c.waiting == nil {
		c.waiting = make(map[string]chan machineCall)
	}
	answers := make(chan machineCall, 1)
	c.waiting[id] = answers
	return answers, func() {
		c.mu.Lock()
		delete(c.waiting, id)
		c.mu.Unlock()
	}, nil
}

// deliver hands one answer to its waiter. IT MUST NEVER BLOCK: its caller is a connection's readLoop,
// which is the sole reader of that socket, and a reader parked here would strand every later message on
// it — including the lease.complete that ends an attempt.
func (c *machineCalls) deliver(data map[string]any) {
	id, _ := data["bg_id"].(string)
	c.mu.Lock()
	answers, waiting := c.waiting[id]
	delete(c.waiting, id)
	c.mu.Unlock()
	if !waiting {
		// Nobody is waiting: the caller gave up, or a machine answered twice. Neither is worth ending a
		// connection over — the id is already unclaimed.
		return
	}
	answers <- machineCall{data: data}
}

// CallMachine asks ONE named machine a bg.* question and waits for its answer.
//
// THE THREE LIFECYCLE VERBS ANSWER DIFFERENTLY, AND THE DIFFERENCES ARE THE POINT rather than an
// accident of where the checks happen to sit:
//
//   - CORDON ANSWERS. A cordon stops new PLACEMENT, not running work; a machine is cordoned precisely so
//     it can be emptied safely. Refusing here would orphan every background task already running on the
//     machine an operator is trying to drain — the opposite of what the verb is for.
//   - DRAIN ANSWERS until the connection goes, which is what a graceful close means. After it goes there
//     is no session and the caller gets ErrMachineUnreachable, which it reports as `lost`.
//   - REVOKE REFUSES. It is the hard stop (SAN-011): the machine is no longer ours, its frames are
//     refused and its sessions are cut. `lost` is the honest answer for a machine we do not own — the
//     process may still be running and we cannot prove it is ours — and it carries the rule that matters,
//     because a lost handle is never signalled.
func (g *RunnerGateway) CallMachine(ctx context.Context, runnerID, verb string, payload map[string]any) (map[string]any, error) {
	pr, err := g.sessionFor(runnerID)
	if err != nil {
		return nil, err
	}
	id := newExecID("bg")
	answers, release, err := g.calls.register(id)
	if err != nil {
		return nil, err
	}
	defer release()

	message := contracts.RunnerMessage{
		Protocol: runner.RunnerProtocolV1,
		Type:     verb,
		Time:     time.Now().UTC().Format(time.RFC3339),
		Data:     runner.BackgroundRequestData(id, payload),
	}
	// NO LEASE IDENTITY ON THIS ENVELOPE, and its absence is a statement rather than an omission: this
	// message is not about an attempt. Filling RunID/AttemptID/Fence with the values of whatever lease
	// the machine happens to hold would name a run that has nothing to do with the task being probed.
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, fmt.Errorf("encode %s: %w", verb, err)
	}
	if err := pr.conn.Write(ctx, websocket.MessageText, encoded); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMachineUnreachable, err)
	}
	select {
	case answer := <-answers:
		if answer.err != nil {
			return nil, answer.err
		}
		if refusal := machineRefusal(answer.data); refusal != nil {
			return nil, refusal
		}
		return answer.data, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("%s on machine %s: no answer before the context ended: %w", verb, runnerID, ctx.Err())
	}
}

// machineRefusal reads one machine answer as a refusal, or nil when the answer is an OUTCOME. It is one
// function rather than an inline check so that every path a bg.* answer can arrive on decodes it the same
// way — the gateway's, and the in-process machine the component fixtures drive.
//
// IT REBUILDS THE ONE TYPED ERROR A CALLER BRANCHES ON. A wire carries strings, and toolbroker.ErrHandleLost
// is not advice: three arms in background.go decide with it — a cancellation settles the row `lost` instead
// of retrying it forever, a deadline does the same, and an explicit kill refuses rather than reporting a
// stranger's process stopped. Flattened to prose, all three fell through to their generic arm, and the rule
// they exist to keep ("a lost handle is never signalled") held only because no signal was sent by the arm
// that had already failed.
//
// The MESSAGE stays the machine's own — the wrapper carries the identity, not the wording — because the
// adapter that raised it is the only side that knows which pgid or which container could not be proven.
func machineRefusal(data map[string]any) error {
	refusal, _ := data["error"].(string)
	if refusal == "" {
		return nil
	}
	if kind, _ := data["error_kind"].(string); kind == runner.BackgroundErrorHandleLost {
		return machineError{msg: refusal, kind: toolbroker.ErrHandleLost}
	}
	return errors.New(refusal)
}

// machineError is a refusal that crossed a wire with its KIND intact: it reads as the machine wrote it and
// answers errors.Is as the sentinel the machine raised.
type machineError struct {
	msg  string
	kind error
}

func (e machineError) Error() string { return e.msg }
func (e machineError) Unwrap() error { return e.kind }

// sessionFor finds a live session belonging to the named machine, and refuses a revoked one.
//
// ANY of its sessions will do. A machine parks one connection per concurrent lease slot
// (PALAI_RUNNER_CONCURRENCY), and they all reach the same kernel — which is the only thing a background
// probe is asking about. Picking the first is therefore not a tie-break that needs an order.
func (g *RunnerGateway) sessionFor(runnerID string) (*pendingRunner, error) {
	if runnerID == "" {
		return nil, fmt.Errorf("%w: no machine was named", ErrMachineUnreachable)
	}
	if g.revoked.Load() {
		return nil, fmt.Errorf("%w: this gateway is revoked", ErrMachineUnreachable)
	}
	g.machinesMu.RLock()
	defer g.machinesMu.RUnlock()
	if life, ok := g.machines[runnerID]; ok {
		if _, revoked, _ := life.state(); revoked {
			return nil, fmt.Errorf("%w: machine %s is revoked", ErrMachineUnreachable, runnerID)
		}
	}
	for pr := range g.sessions {
		if pr.runnerID == runnerID {
			return pr, nil
		}
	}
	return nil, fmt.Errorf("%w: machine %s has no live session", ErrMachineUnreachable, runnerID)
}

// MachineID reports which machine this lease reached. It is the gateway's own minted identity (the
// certificate SAN), never anything the connecting party said.
func (c *gatewayChannel) MachineID() string { return c.pr.runnerID }

// MachineIdentified is the sibling of ExecConn: a channel that can say WHICH machine it reached. An
// attempt learns its machine the same way it learns its executor — from the connection it holds — so a
// background task started during that attempt records a machine the reconciler can address later.
type MachineIdentified interface {
	MachineID() string
}

var _ MachineIdentified = (*gatewayChannel)(nil)

// machineOf reports the machine an attempt's channel reached, or "" for a channel that reached none —
// a bare engine channel, which is what the deterministic e2e tier and every hand-built component
// orchestrator hold. Empty flows through to callMachine, which refuses it.
func machineOf(ch EngineChannel) string {
	identified, ok := ch.(MachineIdentified)
	if !ok {
		return ""
	}
	return identified.MachineID()
}
