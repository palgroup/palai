package tools

import (
	"context"
	"fmt"
	"net/url"
	"strings"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// ShellTool is the built-in workspace shell tool (spec §28.8, SAN-002/003/004). It runs an explicit
// argv — never a shell string, so no metacharacter is interpreted unless the caller opts into shell
// mode — inside the hardened sandbox behind env.Shell: unprivileged user, no network, cgroup
// resource bounds, process-group-killed on teardown, no runtime socket. Output is bounded and
// secret-redacted; any egress target named in the argv is flagged as a finding.
//
// ponytail: argv/shell are declared untyped because the broker's minimal input validator has no
// array/boolean support; the description carries the shape the model reads. A richer JSON-Schema
// validator is the upgrade path if free-choice tool calls need it.
func ShellTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.workspace.shell",
		Description: "Run an explicit command (argv) inside the run's hardened, no-network sandbox and return its bounded, secret-redacted output.",
		ReplayClass: toolbroker.ClassIrreversible, // arbitrary side effects; a kill-after-run must not auto-replay (§26.6, TOL-003)
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"argv":  map[string]any{"description": "command and arguments as a JSON array of strings"},
				"shell": map[string]any{"description": "run through a shell for pipelines/redirection (default false)"},
				// BACKGROUND IS A PARAMETER, NOT A SECOND TOOL, and that is taken verbatim from the harness
				// this replicates: "Claude can set run_in_background: true to start the command as a background
				// task and continue working while it runs" (E26 §3.5 P1). One tool, one extra field, and the
				// model chooses per call — a separate `shell_background` tool would have made the choice a
				// question of which tool to reach for rather than one of how long the command takes.
				"background": map[string]any{"description": "start the command as a background task and return immediately with {task_id, output_path, status} instead of the command's output; read output_path with palai.workspace.file to see how it is going, and palai.workspace.background_kill to stop it (default false)"},
			},
			"required": []any{"argv"},
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         shellExec,
	}
}

// shellExec runs one sandboxed command. It classifies any egress destination named in the argv into
// an audit finding (the sandbox itself denies all egress), then runs the argv through the injected
// sandbox shell runner and returns its bounded, redacted result.
// THE THREE PRE-FLIGHT REFUSALS BELOW ARE ANSWERS (toolbroker.Answer, answer.go) AND THE TWO RUN
// FAILURES BELOW THEM ARE NOT, and the line between them is where the command starts. Nothing has been
// executed at the top of this function, so "there is no shell posture on this deployment", "this run has
// no workspace" and "argv is not an array of strings" are all statements about a call that did not
// happen — the model is told and the run continues. Once env.Shell.Run or StartBackground is entered a
// process may exist, and a process that may exist is exactly what `uncertain` is for, so those keep
// today's abort.
//
// A NON-ZERO EXIT CODE WAS NEVER ON THIS PATH AT ALL and still is not: it is a field of a successful
// result (`exit_code`), which is why `false` completed cleanly while a missing workspace wedged the run.
// That asymmetry was the whole diagnosis, and this is the half of it that changes.
func shellExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	// THE SYNCHRONOUS EXECUTOR IS CHECKED ONLY FOR A SYNCHRONOUS CALL (A.3 T7). It used to be checked
	// here, above everything, which made the BACKGROUND parameter depend on it — and the two answer
	// different questions: this one is "can this attempt run a command and wait", the background branch's
	// is "can this attempt leave a process behind". They gave the same answer while both executors were
	// this process's; they stopped being the same question when each moved to the attempt's machine, and
	// a background call refused for want of a SYNCHRONOUS runner would be refused for the wrong reason.
	//
	// Two branches are now correct where one was: a run whose machine can do neither is refused twice
	// over, once per capability, and each refusal names the capability it is about.
	background, _ := args["background"].(bool)
	if !background && env.Shell == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable,
			"shell tool: no sandbox shell runner wired for this run")
	}
	if env.WorkspaceRoot == "" {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable,
			"shell tool: no workspace bound for this run")
	}
	argv, err := toArgv(args["argv"])
	if err != nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments, "shell tool: %w", err)
	}
	shellMode, _ := args["shell"].(bool)

	// Flag any egress target named in the argv (spec §28.8, SAN-004). The sandbox denies all egress
	// at the network layer; the finding is the audit record that a denied destination was referenced.
	// ponytail: argv-token classification is the heuristic ceiling — real per-connection egress goes
	// through a resolving proxy; the no-network sandbox is the hard backstop.
	var findings []any
	for _, tok := range argv {
		if allowed, f := ClassifyEgress(egressHost(tok)); !allowed && f != nil {
			findings = append(findings, map[string]any{"host": f.Host, "reason": f.Reason})
		}
	}

	cmd := toolbroker.ShellCommand{
		Argv:          argv,
		WorkspaceRoot: env.WorkspaceRoot,
		ReadOnly:      env.ReadOnly,
		Shell:         shellMode,
		// The attempt's environment (E25 T3). It is handed straight to the executor and NEVER read here:
		// this function must not be able to put a value into `findings`, into an error message or into the
		// returned map, all three of which are committed to the tool ledger.
		Env: env.EnvValues,
	}

	// THE BACKGROUND BRANCH (E26 T2). It is the LAST thing this function decides and the whole synchronous
	// path below it is untouched, so a call that never names the parameter runs the same runner with the
	// same command and returns the same fields it always did.
	//
	// Note where this sits: inside Exec, which dispatchTool reaches only AFTER the approval gate. A tool a
	// deployment declared approval_required cannot be spawned by adding a parameter — the human is asked
	// first, and nothing has started while they think about it.
	if background {
		if env.Background == nil {
			// Still pre-flight: the deployment does not do background tasks, and nothing was spawned.
			return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "shell tool: %w", toolbroker.ErrBackgroundUnsupported)
		}
		ticket, err := env.Background.StartBackground(ctx, cmd, env.CallID)
		if err != nil {
			// NOT an answer: a spawn that returned an error may still have left a process behind, and a
			// task nobody is watching is the orphan this seam exists to prevent.
			return nil, fmt.Errorf("shell tool: %w", err)
		}
		out := map[string]any{
			"task_id":     ticket.TaskID,
			"output_path": ticket.OutputPath,
			"status":      string(ticket.State),
		}
		// The egress findings ride along for the same reason they ride the synchronous result: they are the
		// audit record that a denied destination was NAMED, and a command does not stop naming it by being
		// backgrounded.
		if len(findings) > 0 {
			out["egress_findings"] = findings
		}
		return out, nil
	}

	res, err := env.Shell.Run(ctx, cmd)
	if err != nil {
		// NOT an answer, and this is the deliberate one. A missing binary is exit 127 with a result, a
		// wall-time kill is `timed_out: true` with a result — both come back through the success path
		// above. An ERROR here means the runner could not tell us what the command did, which is the one
		// shell outcome that genuinely is uncertain.
		return nil, err
	}

	out := map[string]any{
		"exit_code":   res.ExitCode,
		"stdout":      res.Stdout,
		"stderr":      res.Stderr,
		"truncated":   res.Truncated,
		"timed_out":   res.TimedOut,
		"oom_killed":  res.OOMKilled,
		"duration_ms": res.DurationMS,
	}
	if res.Signal != "" {
		out["signal"] = res.Signal
	}
	if len(findings) > 0 {
		out["egress_findings"] = findings
	}
	return out, nil
}

// egressHost extracts the host an argv token would reach: the host of a URL token, otherwise the
// token itself (a bare host or host:port). It lets the egress classifier see the destination inside
// a `curl http://169.254.169.254/…` form, not just a bare address.
func egressHost(tok string) string {
	if strings.Contains(tok, "://") {
		if u, err := url.Parse(tok); err == nil && u.Host != "" {
			return u.Host
		}
	}
	return tok
}

// toArgv coerces the tool's argv argument — a JSON array of strings (or a Go []string) — into a
// non-empty []string, rejecting a bare string so a shell command is never parsed from an unstructured
// line.
func toArgv(v any) ([]string, error) {
	switch xs := v.(type) {
	case []string:
		if len(xs) == 0 {
			return nil, fmt.Errorf("argv is empty")
		}
		return xs, nil
	case []any:
		out := make([]string, 0, len(xs))
		for _, x := range xs {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("argv element %v is not a string", x)
			}
			out = append(out, s)
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("argv is empty")
		}
		return out, nil
	default:
		return nil, fmt.Errorf("argv must be an array of strings, got %T", v)
	}
}
