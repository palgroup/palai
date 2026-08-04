package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/palgroup/palai/packages/contracts"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// The runner.v1 message types this file adds, and the first pair on that wire addressed to the
// MACHINE rather than to the engine it supervises. Every type that existed before them —
// lease.offer, lease.complete, engine.frame, engine.ready, controller.frame, controller.test — is
// about the engine's lifecycle, and the one control-plane->runner message among them
// (controller.frame) is relayed straight into the engine subprocess's stdin without the runner
// reading its type (stream.go injectControllerFrames). So a tool call could not be expressed at all:
// there was nothing a control plane could say to a machine that the machine itself would act on.
//
// They are a SIBLING of controller.frame, not a subtype of it, because the runner does the opposite
// thing with them: controller.frame is relayed, exec.request is EXECUTED.
//
// The domain is `exec` and not `tool` or `shell` because both of those spellings are already taken
// in this tree by unrelated namespaces, and a wire type that greps together with something else is a
// wire type somebody will eventually confuse: "tool.request"/"tool.result" are ENGINE frame types on
// the engine.v1 protocol (apps/control-plane/internal/execution/tool_dispatch.go:449), and
// "shell.exec" is a worker capability operation name (apps/control-plane/internal/workers). Neither
// travels on runner.v1, and none of the three should answer the same search.
const (
	// ExecRequestType carries one command the control plane asks THIS machine to run.
	ExecRequestType = "exec.request"
	// ExecResultType carries the answer back. It is always sent — see ToolServer.Handle.
	ExecResultType = "exec.result"
	// controllerFrameType is the pre-existing relay message, named here so the branch that now
	// chooses between the two reads as one decision instead of a literal against a constant.
	controllerFrameType = "controller.frame"

	// The BACKGROUND triple (A.3 T7). Siblings of exec.request, not subtypes of it, and the difference
	// is lifetime: an exec.request is answered inside the tool call that made it, while these three are
	// about a process that OUTLIVES its attempt — so they arrive on a connection that may be parked
	// rather than serving a lease, and the control plane routes their answers by machine rather than by
	// attempt.
	//
	// The names were measured free before they were chosen (2026-08-04):
	//
	//	grep -rhoE '"(bg|probe|kill)\.[a-z_]+"' --include='*.go' .   -> (nothing)
	//
	// which is the check `tool.*` failed in T1: that spelling already had sixteen hits on engine.v1, and
	// a wire type that greps together with something else is one somebody eventually confuses.
	BackgroundStartType = "bg.start"
	BackgroundProbeType = "bg.probe"
	BackgroundKillType  = "bg.kill"
	// BackgroundResultType answers all three. ONE reply type rather than three, because the correlation
	// id already says which question is being answered and a second discriminator would be a second
	// thing to keep in step.
	BackgroundResultType = "bg.result"
)

// BackgroundRequestData builds the data payload of a bg.* request. It is the sibling of
// ExecRequestData and carries its correlation the same way — a caller-minted id, because the side that
// must MATCH an answer to a question is the side that names the question.
//
// `bg_id` rather than `exec_id` so a message carrying both would be malformed rather than ambiguous:
// the two pairs travel the same connection and a shared field name would let a stray exec.result
// satisfy a background wait.
func BackgroundRequestData(bgID string, payload map[string]any) map[string]any {
	data := map[string]any{"bg_id": bgID}
	for k, v := range payload {
		if k == "bg_id" {
			continue // the id is this function's to set; a payload cannot rename its own question
		}
		data[k] = v
	}
	return data
}

// ExecRequestData builds the data payload of an exec.request.
//
// CORRELATION IS THIS PAIR'S OWN FIELD and deliberately not contracts.EngineFrame.ReplyTo. That
// field looks like the tree's correlation mechanism and is not one: it is written in exactly one
// production place and compared in none
// (`grep -rn ReplyTo --include='*.go' . | grep -E '==|!=' | wc -l` -> 0, 2026-08-03), so building on
// it would inherit a mechanism nothing implements. It is also the wrong envelope — ReplyTo lives on
// engine.v1 frames, and this pair travels on runner.v1.
//
// The id is minted by the caller (the control plane) rather than by the machine, because the side
// that has to MATCH an answer to a question is the side that must choose the question's name.
func ExecRequestData(execID string, cmd toolbroker.ShellCommand) map[string]any {
	return map[string]any{"exec_id": execID, "command": cmd}
}

// ToolServer runs an exec.request on the machine's own executor. It is the half of A.3 that makes
// "this run on the Mac, that one in a container" expressible: the executor here is the one THIS
// machine was built with, so where a command runs is decided by which machine took the lease.
type ToolServer struct {
	exec toolbroker.ShellRunner
}

// NewToolServer binds the executor this machine runs commands on. A nil executor is a runner that
// was never wired for tool execution; it still answers every request (see Handle).
func NewToolServer(exec toolbroker.ShellRunner) *ToolServer {
	return &ToolServer{exec: exec}
}

// Handle runs one exec.request and returns the exec.result to send back. It carries only Type and
// Data — the lease identity and the runner.v1 envelope are filled by LeaseSession.SendExecResult, so
// there is one place that knows which lease a message belongs to.
//
// IT NEVER RETURNS AN ERROR, AND THAT IS THE POINT. Every path here — a refusal, a malformed
// request, an executor that could not start the process — produces a message. A control plane that
// asked is blocking a tool call on the answer, so silence is not a degraded mode, it is a run that
// never continues. This tree has already paid for the opposite arrangement once, when every tool Exec
// error wedged its run forever.
//
// The two answers are distinguishable and neither is fabricated. A command that RAN answers with
// data.result, including a non-zero exit: a non-zero exit is the shell reporting an outcome, not the
// executor failing, and the seam's own contract says so (adapters/sandboxes/host/exec.go:107). A
// command that did NOT run answers with data.error. Collapsing the second into the first — reporting
// an unwired machine as exit 127, say — would let a misconfiguration read as a command that merely
// failed, which is the more expensive of the two to diagnose.
func (s *ToolServer) Handle(ctx context.Context, request contracts.RunnerMessage) contracts.RunnerMessage {
	execID, _ := request.Data["exec_id"].(string)

	cmd, err := decodeExecCommand(request.Data)
	if err != nil {
		return execRefusal(execID, err)
	}
	if s == nil || s.exec == nil {
		return execRefusal(execID, errors.New("this runner was not wired with an executor, so it can run no command"))
	}

	result, err := s.exec.Run(ctx, cmd)
	if err != nil {
		return execRefusal(execID, err)
	}
	return contracts.RunnerMessage{
		Type: ExecResultType,
		Data: map[string]any{"exec_id": execID, "result": result},
	}
}

func execRefusal(execID string, err error) contracts.RunnerMessage {
	return contracts.RunnerMessage{
		Type: ExecResultType,
		Data: map[string]any{"exec_id": execID, "error": err.Error()},
	}
}

// decodeExecCommand extracts the command an exec.request carries. It re-marshals the field before
// decoding it, the same way decodeRelayFrame reads a relayed engine frame: the value is a
// map[string]any once it has crossed the wire and the struct itself when it has not.
func decodeExecCommand(data map[string]any) (toolbroker.ShellCommand, error) {
	raw, ok := data["command"]
	if !ok {
		return toolbroker.ShellCommand{}, errors.New("exec request carries no command")
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return toolbroker.ShellCommand{}, fmt.Errorf("encode exec command: %w", err)
	}
	var cmd toolbroker.ShellCommand
	if err := json.Unmarshal(encoded, &cmd); err != nil {
		return toolbroker.ShellCommand{}, fmt.Errorf("decode exec command: %w", err)
	}
	return cmd, nil
}

// BackgroundServer answers the bg.* triple on this machine's own background runner. It is the
// background half of what ToolServer is for a synchronous command: the executor here is the one THIS
// machine was built with, so a task started through it is a process on this box and the handle names
// this box's kernel.
type BackgroundServer struct {
	runner toolbroker.BackgroundRunner
}

// NewBackgroundServer binds the detached executor. A nil one is a machine that cannot start a task
// that outlives an attempt; it still answers every request (see Handle).
func NewBackgroundServer(r toolbroker.BackgroundRunner) *BackgroundServer {
	return &BackgroundServer{runner: r}
}

// Handle answers one bg.* request. LIKE ToolServer.Handle IT NEVER RETURNS AN ERROR, and for the same
// reason: a control plane that asked is blocked on the answer, so silence is not a degraded mode, it is
// a sweep that never settles a row and a run that waits forever.
//
// A REFUSAL IS DISTINGUISHABLE FROM AN OUTCOME. data.error means the machine did not do the thing;
// data.status / data.handle mean it did. Collapsing them would let "this machine runs no background
// task" read as "the task is gone", which is the wrong answer this whole seam exists to stop.
func (s *BackgroundServer) Handle(ctx context.Context, request contracts.RunnerMessage) contracts.RunnerMessage {
	bgID, _ := request.Data["bg_id"].(string)
	if s == nil || s.runner == nil {
		return backgroundRefusal(bgID, errors.New("this runner was not wired with a background executor, so it can start and probe no task"))
	}
	switch request.Type {
	case BackgroundStartType:
		cmd, err := decodeExecCommand(request.Data)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		spec, err := decodeBackgroundSpec(request.Data)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		handle, err := s.runner.Start(ctx, cmd, spec)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		return backgroundAnswer(bgID, map[string]any{"handle": handle})
	case BackgroundProbeType:
		handle, err := decodeBackgroundHandle(request.Data)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		status, err := s.runner.Probe(ctx, handle)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		return backgroundAnswer(bgID, map[string]any{"status": status})
	case BackgroundKillType:
		handle, err := decodeBackgroundHandle(request.Data)
		if err != nil {
			return backgroundRefusal(bgID, err)
		}
		if err := s.runner.Kill(ctx, handle); err != nil {
			return backgroundRefusal(bgID, err)
		}
		return backgroundAnswer(bgID, map[string]any{"killed": true})
	}
	return backgroundRefusal(bgID, fmt.Errorf("unknown background verb %q", request.Type))
}

func backgroundAnswer(bgID string, payload map[string]any) contracts.RunnerMessage {
	return contracts.RunnerMessage{Type: BackgroundResultType, Data: BackgroundRequestData(bgID, payload)}
}

func backgroundRefusal(bgID string, err error) contracts.RunnerMessage {
	return contracts.RunnerMessage{Type: BackgroundResultType, Data: BackgroundRequestData(bgID, map[string]any{"error": err.Error()})}
}

// decodeBackgroundSpec and decodeBackgroundHandle re-marshal before decoding, the same way
// decodeExecCommand does: a value is a map[string]any once it has crossed the wire and the struct
// itself when it has not.
func decodeBackgroundSpec(data map[string]any) (toolbroker.BackgroundSpec, error) {
	var spec toolbroker.BackgroundSpec
	return spec, remarshal(data, "spec", &spec)
}

func decodeBackgroundHandle(data map[string]any) (toolbroker.Handle, error) {
	var handle toolbroker.Handle
	return handle, remarshal(data, "handle", &handle)
}

func remarshal(data map[string]any, field string, into any) error {
	raw, ok := data[field]
	if !ok {
		return fmt.Errorf("background request carries no %s", field)
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return fmt.Errorf("encode %s: %w", field, err)
	}
	if err := json.Unmarshal(encoded, into); err != nil {
		return fmt.Errorf("decode %s: %w", field, err)
	}
	return nil
}

// RelayInbound reads control-plane->runner messages for the life of the lease and routes each one by
// type: an engine frame goes to the engine's stdin through inbound, an exec request runs on this
// machine. It closes inbound when the connection ends, which is how the supervisor learns the
// controller stopped sending.
//
// It is EXPORTED so that a test outside this package can drive the routing that ships instead of
// re-spelling it. That is not hypothetical: apps/control-plane/e2e/responses/gateway_parity_test.go
// carried a hand-written copy of this loop, and the copy had already diverged — it could only relay,
// so the parity it proved was the parity of a runner that does not exist. A test that reimplements
// the path it means to measure stays green while the real path changes underneath it.
//
// AN EXEC RUNS IN ITS OWN GOROUTINE SO THE READER KEEPS MOVING. Handling it inline would be simpler
// and wrong: a command is allowed to take minutes (`xcodebuild` is the case this epic exists for),
// and for those minutes nothing else could be read — including the interrupt frames the command pump
// sends mid-run (apps/control-plane/internal/execution/command_pump.go). A stop the operator pressed
// would arrive after the build it was meant to stop. Concurrent writes are safe here: the websocket
// documents every method except Reader/Read as safe for concurrent use
// (github.com/coder/websocket@v1.8.15 conn.go:30), and the reads all happen on this goroutine.
//
// An unknown type still ends the relay, which is the behaviour that shipped before this branch
// existed: the runner has no way to act on a message it cannot name, and continuing would leave the
// control plane waiting on a reply the runner will never form.
func RelayInbound(ctx context.Context, session *LeaseSession, tools *ToolServer, inbound chan<- contracts.EngineFrame, logf func(string, ...any)) {
	RelayInboundServing(ctx, session, MachineServers{Tools: tools}, inbound, logf)
}

// RelayInboundWithBackground is RelayInbound plus the bg.* triple (A.3 T7). The two are separate
// entry points rather than one with a nil argument at every call site, because every caller that
// predates the background triple relays exactly what it always did — a machine with no background
// server answers a bg.* request with a refusal rather than ending the lease on an unknown type.
func RelayInboundWithBackground(ctx context.Context, session *LeaseSession, tools *ToolServer, background *BackgroundServer, inbound chan<- contracts.EngineFrame, logf func(string, ...any)) {
	RelayInboundServing(ctx, session, MachineServers{Tools: tools, Background: background}, inbound, logf)
}

// MachineServers is everything on this machine a lease may be asked to reach: the executor behind
// exec.*, the detached executor behind bg.*, and the disk behind ws.*. It is one struct rather than a
// fourth positional argument because the list grew twice in one epic, and a call site that passes
// three nils in a row is a call site whose next reader cannot tell which nil means what.
//
// EVERY FIELD MAY BE NIL AND NIL IS NEVER SILENCE. Each server answers its own request type with a
// refusal when it is unwired (ToolServer.Handle, BackgroundServer.Handle, WorkspaceServer.Handle), so
// an unconfigured machine tells the control plane so instead of leaving a tool call waiting forever.
type MachineServers struct {
	Tools      *ToolServer
	Background *BackgroundServer
	Workspace  *WorkspaceServer
}

// RelayInboundServing is the relay loop itself. The two functions above are its named shapes.
func RelayInboundServing(ctx context.Context, session *LeaseSession, servers MachineServers, inbound chan<- contracts.EngineFrame, logf func(string, ...any)) {
	tools, background, workspaces := servers.Tools, servers.Background, servers.Workspace
	defer close(inbound)
	for {
		message, err := session.ReceiveMessage(ctx)
		if err != nil {
			return
		}
		switch message.Type {
		case controllerFrameType:
			frame, err := decodeRelayFrame(message.Data)
			if err != nil {
				logf("decode relayed engine frame: %v", err)
				return
			}
			select {
			case inbound <- frame:
			case <-ctx.Done():
				return
			}
		case BackgroundStartType, BackgroundProbeType, BackgroundKillType:
			// THE SAME GOROUTINE ARGUMENT AS AN EXEC, and it is not weaker here. A probe is fast, but a
			// START runs whatever the executor does to spawn a process, and a reader blocked on that is a
			// reader that cannot see the interrupt frame the command pump sends next.
			go func() {
				answer := background.Handle(ctx, message)
				if err := session.SendExecResult(ctx, answer); err != nil {
					logf("send background result: %v", err)
				}
			}()
		case WorkspaceRequestType:
			// THE SAME GOROUTINE ARGUMENT AGAIN, and here it is about a CLONE: `ws.request` carries the
			// §30.3 preparation, which fetches a repository over the network and can take as long as the
			// repository is large. A reader blocked on that is a reader that cannot see the interrupt
			// frame the command pump sends next, so an operator's stop would arrive after the clone it
			// was meant to stop.
			go func() {
				answer := workspaces.Handle(ctx, message)
				if err := session.SendExecResult(ctx, answer); err != nil {
					logf("send workspace result: %v", err)
				}
			}()
		case ExecRequestType:
			go func() {
				answer := tools.Handle(ctx, message)
				if err := session.SendExecResult(ctx, answer); err != nil {
					// The control plane is waiting on this answer and will not get it. Nothing here can
					// repair that — the connection carrying the reply is the one that failed — so it is
					// logged and the lease is left to end on its own read error.
					logf("send exec result: %v", err)
				}
			}()
		default:
			logf("unexpected runner.v1 message type %q", message.Type)
			return
		}
	}
}
