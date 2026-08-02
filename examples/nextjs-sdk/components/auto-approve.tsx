"use client";

import { ChevronDownIcon, GitPullRequestIcon, ShieldCheckIcon, TerminalSquareIcon } from "lucide-react";
import { useState } from "react";

import { Collapsible, CollapsibleContent, CollapsibleTrigger } from "@/components/ui/collapsible";

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
  // Collapsed by default. An operator who has not asked about authorization should not have the
  // top of their conversation spent explaining it to them.
  const [open, setOpen] = useState(false);
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
    <Collapsible
      open={open}
      onOpenChange={setOpen}
      className="border-border border-b"
      data-testid="auto-approve"
    >
      {/* ONE LINE, AND IT STATES THE CURRENT STATE IN WORDS.
          The owner's verdict on the old block was "şurayı kaldır aq sikeceğim bir sike de yaramıyor",
          and the experience earns it even though the feature is correct: three cards sat permanently
          above the conversation reading OFF / OFF / "Nothing is armed" — the top of the column spent on
          three lines saying nothing is happening, on every turn, forever.
          The SPLIT is not deleted, because arming builds while pushes stay off is the safety property
          this whole demo exists to show. It is collapsed. A setting that is not doing anything does not
          get a card; it gets a sentence you can read in one glance and ignore. */}
      <CollapsibleTrigger
        className="flex w-full items-center gap-1.5 px-4 py-2 text-left hover:bg-white/[0.03]"
        data-testid="auto-approve-summary"
      >
        <ShieldCheckIcon className="size-3.5 shrink-0 text-muted-foreground" aria-hidden />
        <span className="min-w-0 flex-1 truncate text-[12px] text-muted-foreground">
          Auto-approve: <span className="text-foreground">{summaryPhrase(armable, state)}</span>
        </span>
        <ChevronDownIcon
          className="size-3.5 shrink-0 text-muted-foreground transition-transform group-data-[state=open]:rotate-180"
          aria-hidden
        />
      </CollapsibleTrigger>

      <CollapsibleContent className="space-y-2 px-4 pt-1 pb-3">

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
        {/* WHICH HALF IS ARMED, NAMED. The line used to read only "Approvals are answered automatically
            under key:key_local" — true for one switch on, both on, and either one, which are four
            different exposures collapsed into one sentence. The SPLIT is the safety property this whole
            control exists for: arming builds while pushes stay off is the state an operator actually
            wants, and it is worth nothing if the screen cannot distinguish it from having armed both.
            Same class as "unset" vs "this process holds no copy" — two states an operator must be able
            to tell apart before they can trust either. */}
        {!armable
          ? "Send a message first — a session is opened on the first turn."
          : state.tools || state.publications
            ? `${armedPhrase(state.tools, state.publications)} — answered automatically under ${state.setBy || "an unidentified principal"}. ${unarmedPhrase(state.tools, state.publications)} Each automatic decision still becomes a decided row on the approvals surface.`
            : "Nothing is armed. Every gated call and every push parks the run and waits for you."}
      </p>

      {/* WHICH CALLS THIS SWITCH ACTUALLY GATES, because the screen was inviting a wrong conclusion.
          MEASURED 2026-08-02: a full iOS build ran to completion with BOTH switches reading OFF. That
          is correct behaviour and it looks exactly like a broken switch — an operator reads "nothing
          is armed" beside a build that just happened and concludes the control does nothing.

          The gate is per-tool and declared at REGISTRATION: `tool_revisions.approval_required`, set by
          an operator beside the per-tool publish. It cannot be inferred from the tool itself — the MCP
          specification says clients "MUST consider tool annotations to be untrusted unless they come
          from trusted servers", so `destructiveHint` is not a gate and nothing auto-classifies.
          The built-in workspace shell carries no such flag, so a build is UNGATED on this deployment
          and the switch would not have stopped it. Saying so is the difference between a control an
          operator can trust and one they quietly learn to ignore. */}
      <p className="text-[11px] text-muted-foreground/80 leading-4" data-testid="auto-approve-scope">
        A tool is gated only if it was registered with <code>approval_required</code>. The built-in
        workspace shell is not, so <strong className="font-medium">a build runs either way</strong> —
        this switch decides gated tool calls, and the one beside it decides pushes.
      </p>

      {refusal !== "" ? (
        <p
          data-testid="auto-approve-refusal"
          className="rounded-md border border-destructive/40 bg-destructive/10 px-2 py-1 text-[11px] text-destructive"
        >
          {refusal}
        </p>
      ) : null}
      </CollapsibleContent>
    </Collapsible>
  );
}

// summaryPhrase is the collapsed line, and it NAMES THE HALVES. "Approvals are answered automatically
// under key:key_local" was true of tools-armed, pushes-armed and both-armed alike — three different
// exposures behind one sentence, which is the C16 defect. "build commands armed · pushes ask" is legible
// at a glance; "nothing is armed" beside a build that just ran is not, which is why the expanded panel
// carries the sentence explaining that a build is ungated here either way.
function summaryPhrase(armable: boolean, state: AutoApproveState): string {
  if (!armable) return "not yet — a session opens on the first turn";
  if (state.tools && state.publications) return "build commands armed · pushes armed";
  if (state.tools) return "build commands armed · pushes ask";
  if (state.publications) return "pushes armed · build commands ask";
  return "nothing armed — every gated call and every push asks";
}

// Switch is a plain button with `role="switch"`, not a checkbox: the control turns a standing
// authorization on and off, and `aria-checked` on a switch is what a screen reader announces as
// "on"/"off" rather than "checked". `aria-busy` covers the in-flight moment, because the state it
// reports is the SERVER's and it is briefly unknown.
// armedPhrase names the half that IS armed, and unarmedPhrase names the half that is not. They are two
// functions rather than one ternary because the "one on, one off" case is the one an operator has to be
// able to read at a glance, and it is the case a single sentence kept flattening.
function armedPhrase(tools: boolean, publications: boolean): string {
  if (tools && publications) return "BOTH gated tool calls AND pushes are armed";
  if (tools) return "Gated tool calls are armed";
  return "Pushes and pull requests are armed";
}

// The unarmed half is stated POSITIVELY rather than left to inference. "Pushes still wait for you" is
// the sentence that makes arming builds a safe thing to do, and silence about it is what made the old
// line read as though everything had been opened at once.
function unarmedPhrase(tools: boolean, publications: boolean): string {
  if (tools && publications) return "Nothing waits for a human.";
  if (tools) return "Pushes and pull requests still park the run and wait for you.";
  return "Gated tool calls still park the run and wait for you.";
}

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
