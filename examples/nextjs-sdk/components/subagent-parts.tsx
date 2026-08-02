"use client";

import { Badge } from "@/components/ui/badge";

// SUBAGENTS ON THE SCREEN. The owner asked to SEE that the agent delegates, and the reason that is worth
// a component rather than a log line is a failure mode rather than a feature:
//
//   an agent that spawned four children and is waiting on them looks EXACTLY like an agent that is
//   stuck, unless the delegation is visible.
//
// The parent's stream carries the delegation and nothing else — a child runs in its own session, so its
// model steps and tool calls are never in this timeline. That is a real limit and this component states
// it rather than implying the child's work is being shown.

export interface ChildPart {
  id: string | null;
  requestId: string | null;
  state: "requested" | "completed" | "denied";
  status?: string | null;
  reason?: string | null;
}

export function SubagentPart({ child }: { child: ChildPart }) {
  // A DENIAL IS NOT THE CHILD FAILING. It is the admission layer refusing to start one — a budget, a
  // depth limit, a policy — so it reads as a refusal with its reason rather than as an error, which
  // would send a reader looking at the child's output for a cause that is not there.
  const tone =
    child.state === "denied" ? "destructive" : child.state === "completed" ? "secondary" : "outline";
  const label =
    child.state === "requested"
      ? "delegated"
      : child.state === "completed"
        ? `finished${child.status ? ` · ${child.status}` : ""}`
        : "refused";

  return (
    <div
      className="flex items-center gap-2 rounded-md border border-dashed px-3 py-2 text-[13px]"
      data-testid="subagent"
      data-state={child.state}
    >
      <span className="font-medium">subagent</span>
      <Badge variant={tone} className="text-[11px]" data-testid="subagent-state">
        {label}
      </Badge>
      {/* The id is the handle a reader needs to go look at that run; truncated because the full one is
          noise in a chat and the prefix is enough to match against a session list. */}
      {child.id ? (
        <span className="font-mono text-[11px] text-muted-foreground" data-testid="subagent-id">
          {child.id.slice(0, 16)}…
        </span>
      ) : null}
      {child.state === "denied" && child.reason ? (
        <span className="text-[12px] text-muted-foreground" data-testid="subagent-reason">
          {child.reason}
        </span>
      ) : null}
      {/* SAYING WHAT IS NOT HERE. Without this line a reader reasonably assumes the absence of the
          child's steps means it did nothing. */}
      {child.state === "requested" ? (
        <span className="text-[12px] text-muted-foreground">its own steps run in its own session</span>
      ) : null}
    </div>
  );
}
