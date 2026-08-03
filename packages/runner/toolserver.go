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
)

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

// relayInbound reads control-plane->runner messages for the life of the lease and routes each one by
// type: an engine frame goes to the engine's stdin through inbound, an exec request runs on this
// machine. It closes inbound when the connection ends, which is how the supervisor learns the
// controller stopped sending.
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
func relayInbound(ctx context.Context, session *LeaseSession, tools *ToolServer, inbound chan<- contracts.EngineFrame, logf func(string, ...any)) {
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
