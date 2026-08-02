"use client";

import { GitPullRequestIcon, ShieldCheckIcon, TerminalSquareIcon } from "lucide-react";
import { useState } from "react";

import { cn } from "@/lib/utils";

// =============================================================================================
// THE SESSION'S STANDING AUTHORIZATION, ON THE SCREEN.
//
// The owner asked for this: "ayrıca auto-approve yapabilmek istiyorum session'u". Watching an agent
// drive `xcodebuild` means answering the same question forty times, and the point of arming a
// session is to stop being asked.
//
// TWO SWITCHES, NEVER ONE, AND THE SCREEN IS WHERE THAT DESIGN HAS TO BE LEGIBLE. Auto-approving a
// gated TOOL call runs a build command in the run's own workspace while somebody watches.
// Auto-approving a PUBLICATION writes to somebody's repository, and that write outlives the session,
// the chat window and the sitting. An operator who wanted the first must never discover they were
// given the second, so:
//
//   * they are separate controls with separate labels and separate consequences in words;
//   * the publication one carries its own warning and is styled as the heavier decision;
//   * the rendered state comes from THE SERVER'S re-read projection, never from what this component
//     optimistically asked for. A toggle that shows its own request rather than the recorded row is
//     a toggle that lies exactly when the request was refused — and a refusal is not hypothetical
//     here: a project whose `approvers` list does not name the arming principal records nothing.
//
// AND IT SAYS WHOSE AUTHORITY IT IS. `auto_approve_set_by` is not decoration: the control plane
// makes the auto-decision AS that principal and still checks the project's approver policy for
// them. Showing the name is what makes "approved automatically" an auditable sentence rather than
// an anonymous one.
// =============================================================================================

export interface AutoApproveState {
  tools: boolean;
  publications: boolean;
  setBy: string;
}

export function AutoApproveControls({
  sessionId,
  state,
  onChange,
  disabled,
}: {
  sessionId: string | null;
  state: AutoApproveState;
  onChange: (next: AutoApproveState) => void;
  disabled?: boolean;
}) {
  const [busy, setBusy] = useState<null | "tools" | "publications">(null);
  const [refusal, setRefusal] = useState("");

  // A session does not exist until the first turn opens one, so there is nothing to arm yet. Saying
  // so beats rendering a dead control that silently does nothing when pressed.
  const armable = sessionId !== null && sessionId !== "";

  async function set(half: "tools" | "publications", value: boolean) {
    if (!armable || busy !== null) return;
    setBusy(half);
    setRefusal("");
    try {
      const res = await fetch("/api/palai/auto-approve", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        // ONLY THE HALF THAT CHANGED IS SENT. The other is omitted, not sent as its current value:
        // absent means "not touching that half" in the published schema, and sending a stale copy
        // would let a click on one control overwrite a change made to the other.
        body: JSON.stringify({ sessionId, [half]: value }),
      });
      const payload = (await res.json()) as Record<string, unknown>;
      if (!res.ok) {
        setRefusal(typeof payload.detail === "string" ? payload.detail : `HTTP ${res.status}`);
        return;
      }
      onChange({
        tools: payload.auto_approve_tools === true,
        publications: payload.auto_approve_publications === true,
        setBy: typeof payload.auto_approve_set_by === "string" ? payload.auto_approve_set_by : "",
      });
    } catch (error) {
      setRefusal(error instanceof Error ? error.message : "the request failed");
    } finally {
      setBusy(null);
    }
  }

  return (
    <div className="space-y-2 border-border border-b px-4 py-3" data-testid="auto-approve">
      <div className="flex items-center gap-1.5">
        <ShieldCheckIcon className="size-3.5 text-muted-foreground" aria-hidden />
        <h3 className="font-medium text-[13px]">Auto-approve this session</h3>
      </div>

      <Switch
        testId="auto-approve-tools"
        icon={<TerminalSquareIcon className="size-3.5 shrink-0" aria-hidden />}
        label="Build commands"
        detail="xcodebuild, simctl and other gated tools run without asking. They act inside this run's workspace."
        checked={state.tools}
        busy={busy === "tools"}
        disabled={disabled || !armable}
        onToggle={(v) => set("tools", v)}
      />

      <Switch
        testId="auto-approve-publications"
        icon={<GitPullRequestIcon className="size-3.5 shrink-0" aria-hidden />}
        label="Pushes and pull requests"
        // THE WARNING IS THE CONTROL'S OWN TEXT, not a tooltip. The whole reason this is a second
        // switch is that its blast radius leaves the machine, and a label that read like the one
        // above it would make the split invisible at the only moment it matters.
        detail="Writes to the repository with NO human in the loop. That write outlives this session."
        heavy
        checked={state.publications}
        busy={busy === "publications"}
        disabled={disabled || !armable}
        onToggle={(v) => set("publications", v)}
      />

      <p className="text-[11px] text-muted-foreground leading-4" data-testid="auto-approve-state">
        {!armable
          ? "Send a message first — a session is opened on the first turn."
          : state.tools || state.publications
            ? `Approvals are answered automatically under ${state.setBy || "an unidentified principal"}. Each one still becomes a decided row on the approvals surface.`
            : "Nothing is armed. Every gated call and every push parks the run and waits for you."}
      </p>

      {refusal !== "" ? (
        <p
          data-testid="auto-approve-refusal"
          className="rounded-md border border-destructive/40 bg-destructive/10 px-2 py-1 text-[11px] text-destructive"
        >
          {refusal}
        </p>
      ) : null}
    </div>
  );
}

// Switch is a plain button with `role="switch"`, not a checkbox: the control turns a standing
// authorization on and off, and `aria-checked` on a switch is what a screen reader announces as
// "on"/"off" rather than "checked". `aria-busy` covers the in-flight moment, because the state it
// reports is the SERVER's and it is briefly unknown.
function Switch({
  testId,
  icon,
  label,
  detail,
  checked,
  busy,
  disabled,
  heavy,
  onToggle,
}: {
  testId: string;
  icon: React.ReactNode;
  label: string;
  detail: string;
  checked: boolean;
  busy: boolean;
  disabled?: boolean;
  heavy?: boolean;
  onToggle: (value: boolean) => void;
}) {
  return (
    <button
      type="button"
      role="switch"
      aria-checked={checked}
      aria-busy={busy}
      disabled={disabled || busy}
      data-testid={testId}
      onClick={() => onToggle(!checked)}
      className={cn(
        "flex w-full items-start gap-2 rounded-md border px-2.5 py-2 text-left transition-colors",
        "disabled:cursor-not-allowed disabled:opacity-50",
        checked
          ? heavy
            ? "border-destructive/50 bg-destructive/10"
            : "border-brand/50 bg-brand/10"
          : "border-border hover:bg-muted/50",
      )}
    >
      <span className={cn("mt-0.5", checked && heavy ? "text-destructive" : checked ? "text-brand" : "text-muted-foreground")}>
        {icon}
      </span>
      <span className="min-w-0 flex-1">
        <span className="flex items-center gap-2">
          <span className="font-medium text-[12px]">{label}</span>
          <span
            className={cn(
              "ml-auto rounded-full px-1.5 py-0.5 text-[10px] tabular-nums",
              checked ? (heavy ? "bg-destructive/20 text-destructive" : "bg-brand/20 text-brand") : "bg-muted text-muted-foreground",
            )}
          >
            {busy ? "…" : checked ? "ON" : "OFF"}
          </span>
        </span>
        <span className="mt-0.5 block text-[11px] text-muted-foreground leading-4">{detail}</span>
      </span>
    </button>
  );
}
