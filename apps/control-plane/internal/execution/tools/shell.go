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
		Name: "palai.workspace.shell",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"argv":  map[string]any{"description": "command and arguments as a JSON array of strings"},
				"shell": map[string]any{"description": "run through a shell for pipelines/redirection (default false)"},
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
func shellExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	if env.Shell == nil {
		return nil, fmt.Errorf("shell tool: no sandbox shell runner wired for this run")
	}
	if env.WorkspaceRoot == "" {
		return nil, fmt.Errorf("shell tool: no workspace bound for this run")
	}
	argv, err := toArgv(args["argv"])
	if err != nil {
		return nil, fmt.Errorf("shell tool: %w", err)
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

	res, err := env.Shell.Run(ctx, toolbroker.ShellCommand{
		Argv:          argv,
		WorkspaceRoot: env.WorkspaceRoot,
		ReadOnly:      env.ReadOnly,
		Shell:         shellMode,
	})
	if err != nil {
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
