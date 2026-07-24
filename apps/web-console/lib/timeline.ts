// laneFor maps a canonical event type to one of the §47.2 timeline lanes the console renders SEPARATELY:
// canonical messages / model steps / tool+subagent activity / approvals / usage / recovery+attempt
// transitions / progress / terminal result. It is a pure prefix table shared by the server relay
// (projection) and the browser timeline (grouping) — no credential path, so it is safe on both sides.
// Keeping the separation in ONE function means the model's prose (a model_step) never lands in the same
// lane as an authoritative approval or a recovery transition (UI-002 / §47.2).

export type Lane = "message" | "model_step" | "tool" | "approval" | "recovery" | "usage" | "progress" | "terminal";

export function laneFor(type: string): Lane {
  if (type === "message.accepted.v1") return "message";
  if (type.startsWith("model_step.")) return "model_step";
  if (type.startsWith("tool_call.") || type.startsWith("child.")) return "tool";
  if (type.startsWith("approval.")) return "approval";
  if (
    type.startsWith("attempt.") ||
    type.startsWith("checkpoint.") ||
    type === "recovery.proof.v1" ||
    type === "host.quarantined.v1" ||
    type === "workspace.recovering.v1" ||
    type === "workspace.restoring.v1" ||
    type === "workspace.restored.v1" ||
    type === "workspace.host_lost.v1"
  ) {
    return "recovery";
  }
  if (type === "usage.updated.v1") return "usage";
  if (isTerminal(type)) return "terminal";
  return "progress";
}

// isTerminal is the run/response terminal set — the lane that carries the authoritative outcome.
export function isTerminal(type: string): boolean {
  return (
    type === "run.completed.v1" ||
    type === "run.failed.v1" ||
    type === "run.canceled.v1" ||
    type === "run.timed_out.v1" ||
    type === "run.budget_exceeded.v1" ||
    type === "response.completed.v1" ||
    type === "response.failed.v1" ||
    type === "response.canceled.v1" ||
    type === "response.timed_out.v1" ||
    type === "response.budget_exceeded.v1"
  );
}
