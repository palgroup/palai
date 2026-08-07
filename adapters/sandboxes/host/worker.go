package host

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"github.com/palgroup/palai/packages/macagent"
	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// workerDialGrace bounds how long a dial and one exchange may take BEYOND the command's own wall time.
// It is additive rather than absolute: a build legitimately runs for the executor's whole window, and a
// deadline that did not account for it would kill every long command at the transport instead of at the
// bound the operator configured.
const workerDialGrace = 30 * time.Second

// dialWorker is how a session worker is reached. It is a field on nothing and a package variable here
// so a test can answer for a worker that exists without one running, and production has exactly one
// implementation.
var dialWorker = func(socket string) (net.Conn, error) {
	return net.Dial("unix", socket)
}

// runThroughWorker forwards one command to the process that is ALREADY the session's account.
//
// ‼️ THIS IS THE ONLY PLACE A SESSION'S COMMAND CROSSES A PROCESS BOUNDARY, and it exists because the
// alternative is arithmetic rather than policy: only uid 0 may become another uid, and this executor is
// the operator. The refusal it replaces was correct and complete — the command really could not be run
// as the account — but it made a deployment that minted per-session accounts refuse every command
// instead of running it anywhere, which is the state this tree was in until 2026-08-08.
//
// THE ENVIRONMENT AND THE ARGV ARE BUILT HERE AND SENT COMPLETE. Nothing about what a command may see is
// decided on the far side: allowedEnv's allow-list, the attempt's own values and the reserved-name
// refusal all run in this process, exactly as they do on the local path, so the two postures cannot
// diverge about what an agent's shell can read.
//
// AND THE RESULT IS REDACTED HERE TOO. The worker returns raw bytes on purpose — a second redactor on
// the far side would be a second place those rules are written, and this tree has measured what happens
// when two copies of one rule disagree.
func (e *Executor) runThroughWorker(ctx context.Context, cmd toolbroker.ShellCommand, runArgv []string, dropErr error) (toolbroker.ShellResult, error) {
	slot, ok := macagent.SlotFromUID(cmd.RunAs.UID)
	if !ok {
		return toolbroker.ShellResult{}, dropErr
	}
	socket, err := macagent.WorkerSocket(slot)
	if err != nil {
		return toolbroker.ShellResult{}, dropErr
	}

	env, err := allowedEnv(cmd.WorkspaceRoot, cmd.RunAs)
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	env, err = toolbroker.LayerEnv(env, reservedEnvNames(), cmd.Env)
	if err != nil {
		return toolbroker.ShellResult{}, err
	}
	envValues := toolbroker.EnvValueList(cmd.Env)

	req := macagent.WorkerRequest{Dir: cmd.WorkspaceRoot, Argv: runArgv, Env: env}
	if e.wallTime > 0 {
		req.TimeoutMS = int(e.wallTime / time.Millisecond)
	}

	start := time.Now()
	resp, err := macagent.AskWorker(func() (net.Conn, error) { return dialWorker(socket) }, req, e.wallTime+workerDialGrace)
	if err != nil {
		// ‼️ BOTH FAILURES IN ONE SENTENCE, because they have different cures and the operator cannot
		// tell them apart from either alone: this process is not root (which is normal and permanent),
		// and the worker that was supposed to make that irrelevant could not be reached.
		return toolbroker.ShellResult{}, fmt.Errorf("%w; and the session worker at %s could not run it either: %v",
			dropErr, socket, err)
	}

	redact := func(s string) string {
		s = toolbroker.RedactValues(toolbroker.RedactSecrets(s), envValues)
		s = toolbroker.RedactValues(s, toolbroker.HostSecretValues())
		return toolbroker.RedactValues(s, toolbroker.HostSecretFileValues())
	}
	stdout, outTrunc := capString(resp.Stdout, e.maxStdoutBytes)
	stderr, errTrunc := capString(resp.Stderr, e.maxStderrBytes)
	result := toolbroker.ShellResult{
		Stdout:     redact(stdout),
		Stderr:     redact(stderr),
		ExitCode:   resp.ExitCode,
		Truncated:  outTrunc || errTrunc,
		DurationMS: time.Since(start).Milliseconds(),
	}
	// A command the worker could not START is 127 on this path for the same reason it is on the local
	// one: a shell reports a missing command that way, and the two postures must read the same.
	if resp.Error != "" && strings.Contains(resp.Error, os.ErrNotExist.Error()) {
		result.ExitCode = 127
		result.Stderr = redact(strings.TrimSpace(stderr + "\n" + runArgv[0] + ": command not found"))
	}
	if ctx.Err() != nil {
		result.TimedOut = true
		result.Signal = "KILL"
	}
	return result, nil
}

// capString applies the same bound the local path's cappedBuffer applies, and reports whether it bit.
// The bound is enforced HERE rather than trusted from the worker for the reason redaction is: a limit
// the far side owned would be a limit that could disagree with the one this executor advertises.
func capString(s string, limit int) (string, bool) {
	if limit <= 0 || len(s) <= limit {
		return s, false
	}
	return s[:limit], true
}
