package tools

import (
	"context"
	"fmt"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// TaskTool is the built-in durable task registry tool (spec §11, master plan line 410). Unlike the
// Claude Agent SDK's ephemeral todo, a task here is DB-backed and session-scoped: it survives a
// context reset and is visible to every attached client through the ordered event journal, so the
// model reads back "what is done and what is not" from durable state (REG-001/002). It upserts a task
// keyed by "key" within the session, or lists the current tasks with action:"list".
func TaskTool() toolbroker.Tool { return registryTool("palai.task", "task") }

// TodoTool is the todo variant of the durable registry — the same durable primitive with kind "todo",
// the resumable/observable form of an agent checklist.
func TodoTool() toolbroker.Tool { return registryTool("palai.todo", "todo") }

// registryTool builds a durable registry tool bound to a kind (task|todo). Both share the registry
// seam; the tool injects its kind so one store serves both. The result is always the CURRENT durable
// list, so the model never guesses at state it can read.
//
// ponytail: fields are declared untyped because the broker's minimal input validator has no
// object/array support; the description carries the shape the model reads (same ceiling as the shell
// tool's argv). A richer JSON-Schema validator is the upgrade path if free-choice calls need it.
func registryTool(name, kind string) toolbroker.Tool {
	return toolbroker.Tool{
		Name:        name,
		Description: "Record, update, or list durable " + kind + "s for this session so progress survives a context reset; returns the current durable list.",
		ReplayClass: toolbroker.ClassIdempotent, // durable session registry, ON CONFLICT idempotent (§26.6)
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action": map[string]any{"description": `"list" to read the current tasks, or omit to add/update one`},
				"key":    map[string]any{"description": "stable identifier for the task within this session (required unless action is list)"},
				"title":  map[string]any{"description": "short human-readable title"},
				"status": map[string]any{"description": `open | in_progress | done | canceled`},
				"detail": map[string]any{"description": "optional metadata object"},
			},
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec: func(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
			if env.Tasks == nil {
				return nil, fmt.Errorf("%s tool: no durable task registry wired for this run", name)
			}
			op := map[string]any{}
			for k, v := range args {
				op[k] = v
			}
			op["kind"] = kind // set AFTER the args: the tool's kind is authoritative, a model-supplied kind cannot override it
			return env.Tasks.ApplyTask(ctx, env.Scope, op)
		},
	}
}
