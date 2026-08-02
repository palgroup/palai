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
import { IOSPart } from "@/components/ios-parts";
import { exitCodeOf, outputTextOf, readIOSOutput, shellCommandOf } from "@/lib/ios-output";
import { cn } from "@/lib/utils";

type Data = Record<string, unknown>;

const s = (v: unknown): string => (typeof v === "string" ? v : "");

// =============================================================================================
// A TOOL CALL, IN THE ECOSYSTEM'S TOOL COMPONENT — AND THE HOLE IS CLOSED
// =============================================================================================
//
// THIS COMPONENT USED TO EXIST TO REPORT A GAP. Measured on the live journal 2026-08-02, the entire
// payload of both tool frames was:
//
//     tool_call.executing.v1 -> {"run_id","replay_class","tool_call_id"}
//     tool_call.completed.v1 -> {"run_id","tool_call_id"}
//
// No name, no arguments, no result — so this card said so, in as many words, rather than printing a
// label the stream never carried. That was the honest thing to do and it was also a permanent hole
// in every rendering of every tool call.
//
// IT IS CLOSED NOW, AND IN TWO PIECES, because the two halves are not the same kind of value:
//   * the NAME rides the frame (E30 T2) — short, and drawn from the closed set of tools the
//     deployment registered — so the card is labelled the moment the call starts;
//   * the ARGUMENTS and RESULT are joined server-side from GET /v1/responses/{id}/tool-calls, because
//     an event payload is POSTed to every registered webhook endpoint and stored immutably per
//     delivery, and a trivial `xcodebuild` build measures 51,422 bytes.
//
// `nameUnavailable` IS STILL HANDLED AND STILL MEANS SOMETHING. A run that started before this
// deployment upgraded — or any frame that arrives without the field — must not be drawn as a tool
// named "". The card falls back to naming the gap, exactly as it used to.
export function ToolPart({ data }: { data: Data }) {
  const id = s(data.id) || "unknown";
  const name = s(data.name);
  const running = s(data.state) === "running";
  const replayClass = s(data.replayClass);
  const named = name !== "";
  // THE OPEN STATE IS CONTROLLED HERE RATHER THAN LEFT TO THE COLLAPSIBLE, and that is a bug fix
  // rather than a preference. `Tool` is an uncontrolled Radix Collapsible; while a run is still
  // streaming, every new part re-renders this subtree and the card an operator had just opened
  // SNAPS SHUT under them. It reproduced as an ordering-dependent test failure — green in isolation,
  // red once the suite left the stream mid-flight — which is exactly how a race announces itself.
  const [open, setOpen] = useState(false);

  return (
    <Tool open={open} onOpenChange={setOpen} data-testid="chat-tool" className="mb-2">
      <ToolHeader
        type="dynamic-tool"
        toolName={named ? name : id}
        title={named ? name : "a tool call — this frame carried no name"}
        state={running ? "input-available" : "output-available"}
        data-testid="chat-tool-header"
      />
      <ToolContent>
        {named ? null : (
          <div className="space-y-2 p-4 pb-0">
            <p data-testid="chat-tool-name-gap" className="text-[13px] text-muted-foreground leading-5">
              <AlertTriangleIcon className="mr-1 inline size-3.5 align-[-2px] text-brand" aria-hidden />
              This frame carried no <strong className="text-foreground">tool_name</strong>. The control
              plane puts one on both tool frames since E30 T2, so this is either a run that started
              before the upgrade or a stack that has not taken it.
            </p>
          </div>
        )}
        <ToolInput input={{ tool_call_id: id, tool_name: name || null, replay_class: replayClass || null }} />
      </ToolContent>
    </Tool>
  );
}

// =============================================================================================
// WHAT THE TOOL ACTUALLY RAN, AND WHAT CAME BACK
// =============================================================================================
//
// The `data-tool-detail` part carries the ledger join: the arguments and the result. For a shell
// call driving `xcodebuild` or `simctl` that is the interesting half of the whole screen, so it is
// handed to the iOS renderer, which decides — from the COMMAND, not from the output — whether this
// was a build, a test run, simulator work, or an ordinary command.
//
// A FAILED JOIN RENDERS AS A FAILED JOIN. The tool card above is already on screen either way; this
// part only adds detail, and saying "the output could not be read" is the difference between an
// honest screen and one that quietly draws an empty successful build.
export function ToolDetailPart({ data }: { data: Data }) {
  if (data.joined !== true) {
    return (
      <p data-testid="chat-tool-detail-unjoined" className="mb-2 text-[12px] text-muted-foreground">
        <AlertTriangleIcon className="mr-1 inline size-3.5 align-[-2px]" aria-hidden />
        This call ran, but its output could not be read
        {s(data.joinNote) !== "" ? `: ${s(data.joinNote)}` : "."}
      </p>
    );
  }

  const command = shellCommandOf(data.arguments);
  // A tool call with no command line is not shell work — a knowledge query, a child run, an MCP
  // write. Drawing it through the iOS renderer would put an Xcode hammer over a Jira ticket.
  if (command === "") {
    return null;
  }

  const report = readIOSOutput(command, outputTextOf(data.result), exitCodeOf(data.result));
  return (
    <div className="mb-2" data-testid="chat-tool-detail" data-kind={report.kind}>
      <IOSPart report={report} />
    </div>
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
