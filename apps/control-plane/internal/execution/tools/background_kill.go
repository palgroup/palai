package tools

import (
	"context"
	"fmt"
	"strings"

	toolbroker "github.com/palgroup/palai/packages/tool-broker"
)

// BackgroundKillTool stops one background task (E26 T2, spec §28.8). It takes ONE argument — the task id
// the shell tool's background call handed back — and it is the only tool this epic adds.
//
// THERE IS NO MATCHING READ TOOL, AND THAT IS THE DESIGN RATHER THAN AN OMISSION. The output of a
// background task is a file inside the run's own allocation and palai.workspace.file already reads files
// under escape control, so a second read path would only be a second thing to keep confined and a second
// place redaction has to be applied. The harness this replicates shipped that second tool and then
// deprecated it: "TaskOutput — Retrieves output from a background task. Deprecated in favor of Read on
// the task's output file path" (E26 §3.5 P2).
//
// ClassIdempotent, because killing a task twice is killing it once. That is the published contract this
// borrows from too ("Cancelling twice is idempotent", §3.5 P7) and it has a mechanical consequence worth
// naming: an idempotent tool takes no pre-write marker and is safely RE-EXECUTED after a kill mid-call,
// which is exactly right here — a re-run of a kill either finds the task gone or kills it again, and both
// are the state the model asked for.
func BackgroundKillTool() toolbroker.Tool {
	return toolbroker.Tool{
		Name:        "palai.workspace.background_kill",
		Description: "Stop a running background task by its task id. Killing a task that has already finished is not an error; read its output with palai.workspace.file on the task's output_path.",
		ReplayClass: toolbroker.ClassIdempotent,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task_id": map[string]any{"type": "string", "description": "the task id returned by a palai.workspace.shell call with background: true"},
			},
			"required":             []any{"task_id"},
			"additionalProperties": false,
		},
		OutputSchema: map[string]any{"type": "object"},
		Exec:         backgroundKillExec,
	}
}

// backgroundKillExec stops one task of THIS run. It resolves nothing itself: the orchestration seam owns
// which handle a task id names and whether this run may name it at all, so a task id belonging to another
// run is refused there rather than trusted here.
func backgroundKillExec(ctx context.Context, env toolbroker.ExecEnv, args map[string]any) (map[string]any, error) {
	// The two pre-flight refusals are ANSWERS (toolbroker.Answer, answer.go): nothing has been signalled,
	// and a model that named no task id can supply one on the next turn. KillBackground's own error is
	// left a fault — the seam may have signalled a process before it failed, and this tool is precisely
	// the one whose "did it happen?" must not be answered by guessing.
	if env.Background == nil {
		return nil, toolbroker.Answerf(toolbroker.AnswerUnavailable, "background kill tool: %w", toolbroker.ErrBackgroundUnsupported)
	}
	taskID, _ := args["task_id"].(string)
	if strings.TrimSpace(taskID) == "" {
		return nil, toolbroker.Answerf(toolbroker.AnswerInvalidArguments, "background kill tool: task_id is required")
	}
	ticket, err := env.Background.KillBackground(ctx, taskID)
	if err != nil {
		return nil, fmt.Errorf("background kill tool: %w", err)
	}
	// The same three fields the spawn returned, so a model reads one shape for one task. `status` is what
	// the operating system says NOW — `exited` for a task that had already finished, which is a normal
	// answer to a kill and not a failure.
	return map[string]any{
		"task_id":     ticket.TaskID,
		"output_path": ticket.OutputPath,
		"status":      string(ticket.State),
	}, nil
}
