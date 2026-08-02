"use client";

import { AlertTriangleIcon, GitPullRequestIcon } from "lucide-react";
import { useState } from "react";

import {
  Confirmation,
  ConfirmationAccepted,
  ConfirmationAction,
  ConfirmationActions,
  ConfirmationRejected,
  ConfirmationRequest,
  ConfirmationTitle,
} from "@/components/ai-elements/confirmation";
import { Tool, ToolContent, ToolHeader, ToolInput } from "@/components/ai-elements/tool";
import { cn } from "@/lib/utils";

type Data = Record<string, unknown>;

const s = (v: unknown): string => (typeof v === "string" ? v : "");

// =============================================================================================
// A TOOL CALL, IN THE ECOSYSTEM'S TOOL COMPONENT, WITH THE HOLE LEFT VISIBLE
// =============================================================================================
//
// AI Elements' ToolHeader wants a NAME. Palai's stream does not have one. Measured on the live
// journal 2026-08-02, and this is the entire payload of both tool frames:
//
//     tool_call.executing.v1 -> {"run_id","replay_class","tool_call_id"}
//     tool_call.completed.v1 -> {"run_id","tool_call_id"}
//
// No name, no arguments, no result. They live on the durable tool_calls ledger, and no HTTP route
// reads it (RunToolCalls has two consumers, both inside execution/changeset.go).
//
// THE FAILURE MODE THIS COMPONENT IS SHAPED TO AVOID. ToolHeader derives its label from `type`:
// pass type="tool-x" and it renders "x". Pass nothing usable and — measured in the vendored source —
// it renders an EMPTY SPAN, silently. A tool card with a blank where the name goes reads like a
// rendering bug; a tool card labelled "tool" reads like a tool actually named that. Both are worse
// than the truth, so `title` is set explicitly to a sentence and the gap is restated in the body.
//
// ToolInput is given the frame's REAL payload rather than a fabricated input. That turns the
// component's "Parameters" section into an accurate statement — this is what Palai sent — instead
// of an empty box.
export function ToolPart({ data }: { data: Data }) {
  const id = s(data.id) || "unknown";
  const running = s(data.state) === "running";
  const replayClass = s(data.replayClass);

  return (
    <Tool defaultOpen={false} data-testid="chat-tool" className="mb-2">
      <ToolHeader
        type="dynamic-tool"
        toolName={id}
        title="a tool call — Palai's stream does not carry its name"
        state={running ? "input-available" : "output-available"}
        data-testid="chat-tool-header"
      />
      <ToolContent>
        <div className="space-y-2 p-4 pb-0">
          <p data-testid="chat-tool-name-gap" className="text-[13px] text-muted-foreground leading-5">
            <AlertTriangleIcon className="mr-1 inline size-3.5 align-[-2px] text-brand" aria-hidden />
            The tool&rsquo;s <strong className="text-foreground">name</strong>, its arguments and its
            result are not carried on Palai&rsquo;s event stream. The whole payload is below. Closing
            this is a control-plane change — put the name on the frame, or give the events API a join
            to the tool_calls ledger — not something this adapter can fix by looking somewhere else.
          </p>
          {replayClass !== "" ? (
            <p className="text-[13px] text-muted-foreground">
              What the frame does say is whether the call was reversible:{" "}
              <span className={cn("font-mono", replayClass === "irreversible" && "text-brand")}>{replayClass}</span>.
            </p>
          ) : null}
        </div>
        <ToolInput input={{ tool_call_id: id, replay_class: replayClass || null }} />
      </ToolContent>
    </Tool>
  );
}

// =============================================================================================
// A PUBLICATION APPROVAL, IN THE ECOSYSTEM'S CONFIRMATION COMPONENT
// =============================================================================================
//
// This is the screen standing between the agent and somebody's branch, and it is the one place the
// demo must be complete rather than suggestive: an operator pressing Approve is authorising a write
// to a real repository under a real identity, and both facts belong on the button.
//
// The AI SDK has no concept for this. Its nearest neighbour, tool approval, is about a tool the
// CLIENT runs; this is a decision the CONTROL PLANE is parked on, and the run stays `waiting` until
// somebody answers. So `Confirmation` is driven from a custom data part, and — measured — it needs a
// server-side join to be filled at all: the approval.requested.v1 frame carries
// {publication_id, operation, branch, request_hash, display} and NOT the remote, the base, the head
// SHA, the credential, or even the approval id. See app/api/chat/route.ts joinApproval.
//
// TWO THINGS THE SCREEN REFUSES TO ROUND OFF:
//   * An empty credential_ref is NOT "unknown". It means the deployment's GitHub App — a different
//     named identity. Rendering a blank would read as "nobody checked".
//   * The request hash is forwarded, never minted here. It is the one-shot binding that makes an
//     approval authorize the exact call proposed; the control plane refuses a body without one.
export function ApprovalPart({ data }: { data: Data }) {
  const [decision, setDecision] = useState<null | boolean>(null);
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState("");

  const approvalId = s(data.approvalId);
  const requestHash = s(data.requestHash);
  const joined = data.joined === true;
  const decidable = approvalId !== "" && requestHash !== "" && decision === null;

  async function decide(approve: boolean) {
    setBusy(true);
    setRefusal("");
    try {
      const res = await fetch("/api/chat/approve", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ approvalId, requestHash, approve }),
      });
      if (res.ok) {
        setDecision(approve);
        return;
      }
      // 403 (not an approver), 409 (no longer decidable) and 404 (unknown) have three different
      // fixes; the relay passes the status through so this can say which one it was.
      const body = (await res.json().catch(() => ({}))) as { detail?: string };
      setRefusal(body.detail ?? `HTTP ${res.status}`);
    } catch (err) {
      setRefusal(err instanceof Error ? err.message : "the decision could not be relayed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <Confirmation
      approval={{ id: approvalId || "pending", ...(decision !== null ? { approved: decision } : {}) }}
      state={decision === null ? "approval-requested" : "approval-responded"}
      className="my-2 border-brand/40"
      data-testid="chat-approval"
    >
      <ConfirmationTitle>
        <GitPullRequestIcon className="mr-1.5 inline size-4 align-[-3px] text-brand" aria-hidden />
        <strong className="font-medium">A human decision is needed</strong> — {s(data.operation) || "a publication"}
      </ConfirmationTitle>

      <dl className="grid grid-cols-[max-content_minmax(0,1fr)] gap-x-3 gap-y-1 text-[13px]" data-testid="approval-facts">
        <Row label="remote" value={s(data.remote)} testid="approval-remote" mono />
        <Row label="branch" value={s(data.branch)} testid="approval-branch" mono />
        <Row label="base" value={s(data.baseBranch)} testid="approval-base" mono />
        <Row label="head" value={s(data.headSha)} testid="approval-head" mono />
        {/* WHOSE CREDENTIAL. Two fixed sentences from the control plane, chosen by the same
            condition the publisher branches on — so the screen shows what the pump will use. */}
        <dt className="text-muted-foreground">as</dt>
        <dd className="min-w-0 break-words" data-testid="approval-credential">
          {s(data.credential) !== "" ? (
            <>
              <span className="text-foreground">{s(data.credential)}</span>
              {s(data.credentialRef) !== "" ? (
                <span className="ml-1.5 font-mono text-muted-foreground">{s(data.credentialRef)}</span>
              ) : null}
            </>
          ) : (
            <span className="text-muted-foreground">not readable — see below</span>
          )}
        </dd>
      </dl>

      {!joined ? (
        <p data-testid="approval-join-gap" className="rounded-md border border-brand/30 bg-brand/5 px-2 py-1.5 text-[12px] text-muted-foreground leading-4">
          <AlertTriangleIcon className="mr-1 inline size-3.5 align-[-2px] text-brand" aria-hidden />
          The destination and identity above come from <code>GET /v1/approvals</code>, not from the
          event stream — the <code>approval.requested.v1</code> frame carries no remote, no base, no
          head SHA, no credential and no approval id. That join did not return this row
          {s(data.joinNote) !== "" ? `: ${s(data.joinNote)}` : "."}
        </p>
      ) : null}

      <ConfirmationRequest>
        <p className="text-[12px] text-muted-foreground leading-4">
          Approving forwards the one-shot request hash the run emitted, so this authorises the exact
          call proposed and not whatever it becomes afterwards. The run is parked <code>waiting</code>{" "}
          until somebody answers.
        </p>
      </ConfirmationRequest>

      <ConfirmationAccepted>
        <p data-testid="approval-outcome" className="text-[13px] text-added">
          Approved. The publication moves to <code>approved</code>, the durable command applies, and
          the parked run wakes.
        </p>
      </ConfirmationAccepted>
      <ConfirmationRejected>
        <p data-testid="approval-outcome" className="text-[13px] text-destructive">
          Denied. The reason rides back to the model verbatim.
        </p>
      </ConfirmationRejected>

      {refusal !== "" ? (
        <p data-testid="approval-refusal" className="text-[13px] text-destructive">
          refused: {refusal}
        </p>
      ) : null}

      <ConfirmationActions>
        <ConfirmationAction variant="outline" disabled={!decidable || busy} onClick={() => void decide(false)} data-testid="chat-deny">
          Deny
        </ConfirmationAction>
        <ConfirmationAction disabled={!decidable || busy} onClick={() => void decide(true)} data-testid="chat-approve">
          {busy ? "Applying…" : "Approve"}
        </ConfirmationAction>
      </ConfirmationActions>
    </Confirmation>
  );
}

function Row({ label, value, testid, mono }: { label: string; value: string; testid: string; mono?: boolean }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className={cn("min-w-0 break-all", mono && "font-mono")} data-testid={testid}>
        {value !== "" ? value : <span className="font-sans text-muted-foreground">not carried</span>}
      </dd>
    </>
  );
}

// A published receipt. Small on purpose — the interesting moment was the decision, not the echo.
export function PublicationPart({ data }: { data: Data }) {
  const receipt = data.receipt as Record<string, unknown> | null;
  return (
    <p data-testid="chat-publication" className="my-2 rounded-md border border-added/40 bg-added/5 px-3 py-2 text-[13px]">
      published <strong className="font-medium">{s(data.operation)}</strong>
      {receipt ? (
        <span className="ml-2 font-mono text-[12px] text-muted-foreground">
          {Object.entries(receipt)
            .map(([k, v]) => `${k}=${String(v)}`)
            .join("  ")}
        </span>
      ) : null}
    </p>
  );
}

// A frame the AI SDK protocol has no part for. Rendered rather than dropped: a run that recovered
// from a crash, or one parked forever on an unresolved tool call, is exactly what an operator needs
// to be told, and silence is the one answer that misleads.
export function NoticePart({ data }: { data: Data }) {
  const error = s(data.level) === "error";
  return (
    <p
      data-testid="chat-notice"
      className={cn(
        "my-2 rounded-md border px-3 py-2 text-[13px] leading-5",
        error ? "border-destructive/40 bg-destructive/10 text-destructive" : "border-brand/30 bg-brand/5 text-ink-dim",
      )}
    >
      {s(data.text)}
    </p>
  );
}
