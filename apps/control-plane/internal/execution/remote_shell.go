package execution

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

// The control plane's end of the A.3 exec wire: a toolbroker.ShellRunner that runs its command on the
// MACHINE THAT HOLDS THE ATTEMPT'S LEASE instead of on the control plane's own host. Which machine a
// command runs on stops being a property of the deployment and becomes a property of the lease, which
// is what makes "this run on a Mac, that one in a container" expressible.
//
// IT IS NOT BUILT ON EngineChannel, and that is the load-bearing decision rather than a style
// preference. gatewayChannel.Send wraps every EngineFrame in a controller.frame, whose destination on
// the far side is the engine subprocess's STDIN (packages/runner/stream.go injectControllerFrames) —
// a command sent that way would reach the engine, never the machine. And EngineChannel.Receive has a
// single consumer whose sequence discipline lives on attemptState, not on the channel
// (orchestrator.go receiveEngineFrame): a second reader there would swallow a frame the orchestrator
// then reports as a sequence gap, failing the attempt with a protocol violation.
//
// So the exec pair is a SIBLING of the relay on the same connection, and the two halves below split
// along the one seam that matters: the connection already has a sole reader, so the ANSWER is routed
// by that reader (gatewayChannel, the methods in this file), and only the policy around it — minting
// the id, waiting, turning the wire's two answer shapes into the two ShellRunner ones — belongs to
// RemoteShell.

// errLeaseConnectionEnded is what a command still waiting on its machine is told when the lease
// connection ends before the answer arrives. It is an error rather than a result because it is
// genuinely uncertain: the command may well have run, and the connection that would have said so is
// the one that failed. tools/shell.go treats exactly this case as the uncertain one.
var errLeaseConnectionEnded = errors.New("the machine's lease connection ended before the command answered")

// ExecAnswer is one exec.result, decoded. The two fields mirror the wire's two answer shapes and
// exactly one of them is set: data.result for a command that RAN (including one that exited
// non-zero), data.error for one that did not.
type ExecAnswer struct {
	Result toolbroker.ShellResult
	Err    error
}

// ExecConn is the lease connection RemoteShell asks a machine to run commands over. It is a narrow
// interface rather than *gatewayChannel so this file states what it needs — and so that the exec
// surface is something a caller ASSERTS for, which is the honest shape: Dial returns an EngineChannel,
// and whether that channel can also reach the machine is a fact about the transport, not about every
// engine channel.
type ExecConn interface {
	// StartExec registers execID as awaiting an answer and then sends the request, in that order.
	// The order is the correctness argument: the connection's reader can deliver an answer the
	// instant after the write returns, and a registration made afterwards would race it.
	//
	// It returns the channel the answer arrives on and the release that unregisters the wait, which
	// the caller must call once it has stopped waiting.
	StartExec(ctx context.Context, execID string, cmd toolbroker.ShellCommand) (<-chan ExecAnswer, func(), error)
}

var (
	_ ExecConn               = (*gatewayChannel)(nil)
	_ toolbroker.ShellRunner = (*RemoteShell)(nil)
)

// pendingAnswers is the set of questions one lease has asked its machine and not yet heard back
// about, keyed by the id the control plane minted. It is the whole of the correlation mechanism:
// contracts.RunnerMessage carries no identity field, and contracts.EngineFrame.ReplyTo — the field
// that LOOKS like this tree's correlation — is written in one place and compared in none, besides
// being on the wrong protocol.
//
// IT IS GENERIC BECAUSE THERE ARE NOW TWO KINDS OF QUESTION ON ONE CONNECTION and there was very
// nearly a second copy of it. A command (exec.*) and a workspace operation (ws.*, A.3 T5) correlate
// identically and differ only in what an answer IS; the properties below — the buffered channel, the
// removal under the lock, the refusal after close — are the ones a second hand-written copy would
// eventually get wrong in one of the two.
type pendingAnswers[T any] struct {
	mu      sync.Mutex
	waiting map[string]chan T
	// closed is set once the connection ended, so a question registered after that is refused
	// immediately rather than waiting for an answer that can no longer be delivered.
	closed error
}

// register claims id and returns the channel its answer will arrive on. The channel is buffered so
// that delivering an answer NEVER blocks the caller of deliver — see deliver.
func (p *pendingAnswers[T]) register(id string) (<-chan T, func(), error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed != nil {
		return nil, nil, p.closed
	}
	if _, taken := p.waiting[id]; taken {
		return nil, nil, fmt.Errorf("id %s is already awaiting an answer on this lease", id)
	}
	if p.waiting == nil {
		p.waiting = make(map[string]chan T)
	}
	answers := make(chan T, 1)
	p.waiting[id] = answers
	return answers, func() {
		p.mu.Lock()
		delete(p.waiting, id)
		p.mu.Unlock()
	}, nil
}

// deliver hands one answer to the question waiting for it and reports whether anyone was.
//
// IT MUST NEVER BLOCK, because its caller is the connection's sole reader (readLoop). A reader parked
// here would strand every later message on that connection, the lease.complete included. Two things
// make that true and both are needed: the channel holds one answer, and the entry is removed under
// the lock before the send, so this is the only send that entry will ever see.
func (p *pendingAnswers[T]) deliver(id string, answer T) bool {
	p.mu.Lock()
	answers, waiting := p.waiting[id]
	delete(p.waiting, id)
	p.mu.Unlock()
	if !waiting {
		return false
	}
	answers <- answer
	return true
}

// closeAll answers every question still waiting, and refuses later ones, so that "no question outlives
// the connection it was sent on" holds however the lease ended. Calling it twice is harmless: the
// second call finds nothing waiting. Its callers are the gatewayChannel teardown doors, and they all
// go through closeAllPending — see there for why that indirection is not ceremony.
//
// The answer is passed IN rather than built here, because only the caller knows what "the connection
// ended" looks like in its own answer type. That is the price of not writing this registry twice.
func (p *pendingAnswers[T]) closeAll(err error, answer T) {
	p.mu.Lock()
	waiting := p.waiting
	p.waiting = nil
	if p.closed == nil {
		p.closed = err
	}
	p.mu.Unlock()
	for _, answers := range waiting {
		answers <- answer
	}
}

// StartExec asks the machine holding this lease to run one command. The message is a sibling of
// Send's controller.frame on the same connection and in the same runner.v1 envelope — same lease
// identity, same fence — because it is the same lease speaking; only what the far side DOES with it
// differs, and that is the whole of A.3.
func (c *gatewayChannel) StartExec(ctx context.Context, execID string, cmd toolbroker.ShellCommand) (<-chan ExecAnswer, func(), error) {
	answers, release, err := c.execs.register(execID)
	if err != nil {
		return nil, nil, err
	}
	message := contracts.RunnerMessage{
		Protocol:  runner.RunnerProtocolV1,
		Type:      runner.ExecRequestType,
		Time:      time.Now().UTC().Format(time.RFC3339),
		LeaseID:   c.leaseID,
		RunID:     c.attempt.RunID,
		AttemptID: c.attempt.AttemptID,
		Fence:     int(c.attempt.Fence),
		// The request body comes from the runner package that also reads it, so the two ends of this
		// wire cannot drift into two spellings of the same field.
		Data: runner.ExecRequestData(execID, cmd),
	}
	payload, err := json.Marshal(message)
	if err != nil {
		release()
		return nil, nil, fmt.Errorf("encode exec request: %w", err)
	}
	if err := c.pr.conn.Write(ctx, websocket.MessageText, payload); err != nil {
		release()
		return nil, nil, fmt.Errorf("send exec request: %w", err)
	}
	return answers, release, nil
}

// deliverExecResult routes one exec.result to the command that asked for it. readLoop calls it, which
// is the point: the connection already has a reader, so the answer is handed over by that reader
// rather than by a second one.
//
// An answer nobody is waiting for is dropped, and there are two ways to earn one: the caller gave up
// (its context ended, and it released its wait), or a machine answered the same request twice. Neither
// is worth ending the lease over — the first is normal and the second costs nothing, since the id is
// already unclaimed.
func (c *gatewayChannel) deliverExecResult(data map[string]any) {
	execID, _ := data["exec_id"].(string)
	c.execs.deliver(execID, decodeExecAnswer(data))
}

// decodeExecAnswer reads the two shapes ToolServer.Handle produces. data.error is checked first
// because a refusal carries no result at all.
//
// A REFUSAL BECOMES AN ERROR, WHICH IS COARSER THAN THE WIRE. The machine knows the difference between
// "I was never wired with an executor" (nothing ran) and "the executor could not start the process"
// (something may have), and toolbroker.ShellRunner's two return values cannot express it — an error is
// the uncertain one. That loses nothing today only because ToolServer.Handle already merges both into
// data.error; a wire that separated them would need a third answer shape here.
func decodeExecAnswer(data map[string]any) ExecAnswer {
	if refusal, ok := data["error"].(string); ok && refusal != "" {
		return ExecAnswer{Err: errors.New(refusal)}
	}
	raw, ok := data["result"]
	if !ok {
		return ExecAnswer{Err: errors.New("exec result carries neither a result nor an error")}
	}
	// Re-marshal before decoding, the same way decodeRelayFrame and decodeExecCommand do: the value is
	// a map[string]any once it has crossed the wire.
	encoded, err := json.Marshal(raw)
	if err != nil {
		return ExecAnswer{Err: fmt.Errorf("encode exec result: %w", err)}
	}
	var result toolbroker.ShellResult
	if err := json.Unmarshal(encoded, &result); err != nil {
		return ExecAnswer{Err: fmt.Errorf("decode exec result: %w", err)}
	}
	return ExecAnswer{Result: result}
}

// RemoteShell runs a run's shell commands on the machine that holds its lease. It satisfies
// toolbroker.ShellRunner, so tools/shell.go dispatches through it without knowing where the command
// lands — the substitution IS the feature.
type RemoteShell struct {
	conn ExecConn
}

// NewRemoteShell binds one attempt's lease connection. A RemoteShell is therefore attempt-scoped, the
// same as the lease: it can only reach the machine that took this attempt.
func NewRemoteShell(conn ExecConn) *RemoteShell {
	return &RemoteShell{conn: conn}
}

// Run sends one command to the machine and waits for its answer.
//
// A NON-ZERO EXIT IS A RESULT, NOT AN ERROR, and the distinction decides whether a run continues:
// tools/shell.go turns an error here into an uncertain abort that ends the attempt, while a result
// with exit_code 1 is handed to the model. `false` exits 1 and completes cleanly. This tree has
// already paid for the opposite arrangement once, when every tool Exec error wedged its run forever.
//
// There is no timer here on purpose. The command's wall clock is bounded on the MACHINE, by the
// executor that runs it, and a second bound on this side would have to be either shorter than that
// one — killing the `xcodebuild` this epic exists for — or longer, in which case it never fires.
// What ends an unanswerable wait instead is the connection: when the lease drops, every command still
// waiting is answered (execPending.closeAll).
func (s *RemoteShell) Run(ctx context.Context, cmd toolbroker.ShellCommand) (toolbroker.ShellResult, error) {
	execID := newExecID("exec")
	answers, release, err := s.conn.StartExec(ctx, execID, cmd)
	if err != nil {
		return toolbroker.ShellResult{}, fmt.Errorf("ask the machine to run %s: %w", execID, err)
	}
	defer release()

	select {
	case answer := <-answers:
		if answer.Err != nil {
			return toolbroker.ShellResult{}, fmt.Errorf("machine exec %s: %w", execID, answer.Err)
		}
		return answer.Result, nil
	case <-ctx.Done():
		return toolbroker.ShellResult{}, fmt.Errorf("machine exec %s: no answer before the context ended: %w", execID, ctx.Err())
	}
}

// closeAllPending answers every question still waiting on this lease — a command and a workspace
// operation alike — and refuses later ones. It is the ONE call every teardown door makes, so a door
// added later cannot answer half the waiters: a registry left out here is a tool call that waits for
// an answer the connection can no longer carry, which is exactly the wedge this seam exists to
// prevent and exactly the shape a reader would not notice was missing.
func (c *gatewayChannel) closeAllPending(err error) {
	c.execs.closeAll(err, ExecAnswer{Err: err})
	c.workspaces.closeAll(err, WorkspaceAnswer{Err: err})
}
