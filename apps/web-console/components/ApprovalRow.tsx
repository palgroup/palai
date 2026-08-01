"use client";

import { useState } from "react";

import { Button } from "@/components/ui/Button";
import { ResourceForm } from "@/components/ResourceForm";
import { apiSend, RelayError, type Problem } from "@/lib/api";

// ONE PARKED GATED TOOL CALL, as an operator reads it and answers it (E25 T5, plan §T5).
//
// IT IS A DIFFERENT COMPONENT FROM ApprovalPanel AND THE DIFFERENCE IS THE WHOLE OF §3.6 D4. ApprovalPanel is
// a PUBLICATION approval — a push / pull request / merge — which exists only inside a live run's event stream
// (`approval.requested.v1`, packages/coordinator/publication.go) and is answered with a durable COMMAND on
// /v1/sessions/{id}/commands. This is a TOOL approval: a row in a queue that GET /v1/approvals returns, whose
// answer is a decision on /v1/approvals/{id}/approve|deny. Two queues, two shapes, two routes. Folding them
// into one component would have meant one of the two rendering the other's fields as blanks, and a blank on an
// approval screen reads as "there is nothing there" rather than "this is the wrong screen".
//
// THE SCREEN IS NOT RECOMPUTED HERE (§2). `identity`, `operator_label`, `arguments` and `truncated` are
// slack.DeriveApprovalDisplay's output, reaching this surface through internal/store/approvals.go:60 — the SAME
// derivation the Slack message and the Slack modal render, so the two surfaces cannot disagree about what a
// human approved. This component formats none of it: no re-indenting, no JSON re-parse, no truncation of its
// own, no "friendly" name for the identity. Two renderings of one ledger row are byte-identical, and that is
// only true if every renderer refuses to be clever.
//
// THE ARGUMENTS ARE MODEL-AUTHORED TEXT AND THEY ARE RENDERED AS TEXT. React escapes them, and there is no
// dangerouslySetInnerHTML anywhere in this console — asserted twice over (a token scan of every source file,
// and a hostile payload in the fixture's own arguments that must never execute). No link in them is made
// clickable either: a URL the model wrote is not a URL an operator asked for.
//
// AND WHAT IS NOT ON THE WIRE IS WORTH NAMING, because it is the one thing this screen has that T7's
// registration screen does not: there is no MCP `description` on api.PendingApproval at all (approvals.go:
// 52-67). The vendor's own display fields are the ones whose author has an interest in the answer, so the
// approval screen carries the CALL and nothing a server wrote about itself.

export interface ToolApproval extends Record<string, unknown> {
  id?: string;
  object?: string;
  tool_call_id?: string;
  run_id?: string;
  session_id?: string;
  response_id?: string;
  request_hash?: string;
  expires_at?: string;
  created_at?: string;
  identity?: string;
  operator_label?: string;
  arguments?: string;
  truncated?: boolean;
  // THE PUBLICATION FAMILY (2026-08-01). `kind` is "tool" or "publication"; the rest are empty on a tool
  // row. They are on this row because a publication's `arguments` are `{}` — a push carries none — so
  // without them the screen for "may this write leave the machine" would say the operation name and an
  // empty object, and an operator would be approving a write to a repository the screen never names.
  kind?: string;
  publication_id?: string;
  operation?: string;
  remote?: string;
  branch?: string;
  base?: string;
  head_sha?: string;
}

// DENY_NEEDS_A_REASON is the console's OWN requirement, and the asymmetry with the hash below is deliberate.
//
// The API accepts a denial with an empty reason (api.ApprovalDecision.Reason is optional), and the Slack
// surface hands the model a CONSTANT sentence because nothing routes a view_submission there — `HIL-P10`, open
// since E23 T8. On this surface the reason field is wired to ApprovalDecision.Reason, which rides back to the
// model verbatim, so the ceiling is CLOSED here — and it is closed by making the operator write the sentence
// rather than by having a field that could be left blank. A denial with no reason is a wall; a denial with one
// is an instruction the agent can act on.
export const DENY_NEEDS_A_REASON =
  "A denial needs a reason, and it is not decoration: this text is handed to the model verbatim as the answer " +
  "to what it asked for. Write what must not happen and why.";

// refusalText gives each TYPED refusal its own sentence. None of them is "something went wrong", and that is
// the point rather than a nicety: the API distinguishes four causes at api/approvals.go:200-221, an operator's
// next action is different for every one of them, and a console that flattened them would send someone with a
// misconfigured approver list off to "try again".
//
// IT KEYS ON `code` FIRST, AND THAT IS A CORRECTION THE PLAN'S §T5 DOES NOT MAKE. There are not three typed
// refusals on this surface, there are FOUR — because 403 arrives for two independent reasons with two stable
// codes: `insufficient_scope` (authorize(): the KEY does not hold the `approve` capability) and
// `not_an_approver` (the project's `config_policy.approvers` refuses this principal). They are two different
// gates on purpose (`approve` is deliberately not `provision`, so a key that can decide cannot rewrite the
// list it is checked against) and they are two different fixes, so they get two sentences. Status is the
// fallback for a code this console has not met, and even that names the code rather than shrugging.
export function refusalText(problem: Problem): string {
  switch (problem.code) {
    case "invalid_request":
      return (
        "This decision carried no request hash, so it authorized nothing — the server refused it at the edge. " +
        "The console reads the hash out of the row it displayed, so this means the row arrived without one: " +
        "reload the queue and read the call again before deciding."
      );
    case "insufficient_scope":
      return (
        "This console's API key does not hold the `approve` capability. Reading this queue and deciding on it " +
        "are gated on the same capability, so a key that could list these rows and cannot decide them has " +
        "changed underneath you. See docs/operations/console.md §3 for minting one that holds it."
      );
    case "not_an_approver":
      return (
        "This key is not in the project's approver list. That is the SECOND gate, independent of the `approve` " +
        "capability: add this key's principal (`key:<api_key_id>`) to the project's config_policy.approvers, or " +
        "decide with a key that is already in it."
      );
    case "not_found":
      return (
        "This approval no longer exists. An id that was never real and an id belonging to another project are " +
        "deliberately indistinguishable here, so this says nothing about whether it exists somewhere else — and " +
        "a row that was answered elsewhere, or reaped after its deadline, reads the same way. Reload the queue."
      );
    case "approval_not_decidable":
      return (
        "This can no longer be decided: it was already answered, its deadline passed, or the arguments changed " +
        "after this row was rendered — the request hash no longer matches the call that is queued. Nothing was " +
        "authorized. Reload the queue and read the arguments again, because they may not be the ones you read."
      );
    default:
      break;
  }
  switch (problem.status) {
    case 401:
      return "The console's own session was refused. Sign in again — nothing was decided.";
    case 403:
      return `A gate refused this decision (${problem.code}). Nothing was authorized.`;
    case 404:
      return "This approval no longer exists. Reload the queue.";
    case 409:
      return "This can no longer be decided, and nothing was authorized. Reload the queue.";
    default:
      // Deliberately NOT "something went wrong": the code, the status and the server's own words, so an
      // operator can tell a refusal from an outage without reading a log.
      return `The decision was not applied. The server answered ${problem.code} (HTTP ${problem.status})${problem.detail === "" ? "" : `: ${problem.detail}`}`;
  }
}

/** decisionRefusal maps anything thrown by the relay client; a non-HTTP failure still names itself. */
function decisionRefusal(err: unknown): string {
  if (err instanceof RelayError) return refusalText(err.problem);
  return "The decision never reached the control plane, so nothing was authorized. Check the console's connection and read the queue again.";
}

// onDecided is handed the OUTCOME rather than being a bare signal, and that is a bug this component had for one
// test run: an applied decision makes the row LEAVE the queue, so a confirmation rendered inside the row is
// destroyed by the very refetch that proves it worked — the operator clicks, the row vanishes, and nothing on
// screen says what was recorded. It was found by a spec that read the message after the reload rather than
// before it, and the earlier one that read it before was RACILY GREEN. So the outcome is reported UPWARD, to a
// region that outlives the row; a refusal stays local, because a refused row is still there to hold it.
export function ApprovalRow({ approval, onDecided }: { approval: ToolApproval; onDecided: (outcome: string) => void }) {
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");

  const id = String(approval.id ?? "");
  const hash = String(approval.request_hash ?? "");

  // decide sends ONE answer. THE HASH COMES OUT OF THE ROW'S OWN DATA — not from a field the operator types
  // into, not from a hidden input, not recomputed from the arguments. It is the one-shot binding: if the
  // arguments changed after this row was rendered, this hash no longer matches the queued call and the decision
  // authorizes nothing (409). That is the property the whole gate rests on.
  //
  // AN EMPTY HASH IS NOT PRE-CHECKED, ON PURPOSE. The binding is the SERVER's rule (api/approvals.go:200-204)
  // and a client-side guard here would make the honest refusal unreachable — and therefore its rendering
  // untested, which is how a "we handle that" comment becomes the only evidence that anything handles it.
  async function decide(approve: boolean) {
    if (!approve && reason.trim() === "") {
      setError(DENY_NEEDS_A_REASON);
      return;
    }
    setBusy(true);
    setError("");
    try {
      const body = await apiSend<{ decision?: string }>(
        "POST",
        `/approvals/${encodeURIComponent(id)}/${approve ? "approve" : "deny"}`,
        approve ? { request_hash: hash } : { request_hash: hash, reason },
      );
      // The SERVER's word for what happened, not the button's. A console that printed "approved" because the
      // approve button was the one clicked would be reporting its own intention as an outcome.
      onDecided(
        `${approval.identity ?? "the call"} (${id}) was recorded as ${body.decision ?? (approve ? "approved" : "denied")}, ` +
          "against the console's API key. It has left the queue.",
      );
    } catch (err: unknown) {
      setError(decisionRefusal(err));
    } finally {
      setBusy(false);
    }
  }

  return (
    <>
      {/* THE LEDGER'S OWN FACTS, first, because they are what is being authorized. */}
      <dl data-testid={`tool-approval-facts-${id}`}>
        <dt>Tool identity</dt>
        <dd>
          <code data-testid={`tool-approval-identity-${id}`}>{approval.identity ?? ""}</code>
        </dd>
        <dt>What the operator wrote about it at registration</dt>
        <dd data-testid={`tool-approval-operator-label-${id}`}>{approval.operator_label ?? ""}</dd>
        {/* WHERE THE WRITE IS GOING, on a publication row only. None of it is the model's: remote, branch and
            base are resolved from the run's repository binding, which is exactly why a model cannot name the
            destination of its own push. A tool row renders none of this rather than rendering it empty. */}
        {approval.kind === "publication" ? (
          <>
            <dt>Where this write is going</dt>
            <dd data-testid={`tool-approval-destination-${id}`}>
              <code data-testid={`tool-approval-remote-${id}`}>{approval.remote ?? ""}</code>
              {" — pushing "}
              <code data-testid={`tool-approval-branch-${id}`}>{approval.branch ?? ""}</code>
              {" onto base "}
              <code data-testid={`tool-approval-base-${id}`}>{approval.base ?? ""}</code>
              {approval.head_sha ? (
                <>
                  {" at "}
                  <code data-testid={`tool-approval-head-${id}`}>{approval.head_sha}</code>
                </>
              ) : null}
            </dd>
          </>
        ) : null}
        <dt>Arguments — exactly what will be sent</dt>
        <dd>
          {/* A <pre>, because the server's canonical rendering is whitespace-significant and the indentation is
              part of what a human reads. The text is model-authored and stays TEXT. */}
          <pre data-testid={`tool-approval-arguments-${id}`}>{approval.arguments ?? ""}</pre>
          {approval.truncated === true ? (
            <p data-testid={`tool-approval-truncated-${id}`}>
              <span className="glyph" aria-hidden="true">
                ⚠
              </span>{" "}
              <strong>These arguments are CUT.</strong> The server rendered part of the call and said so — the
              full bytes are on the tool call this decision is bound to, and the hash below binds to all of
              them, not to the part shown.
            </p>
          ) : null}
        </dd>
        <dt>Request hash (the one-shot binding)</dt>
        <dd>
          <code data-testid={`tool-approval-request-hash-${id}`}>{hash}</code>
        </dd>
        <dt>Deadline</dt>
        <dd data-testid={`tool-approval-expires-${id}`}>
          {approval.expires_at === undefined
            ? "none — this gate has no deadline, so the run waits until somebody answers"
            : String(approval.expires_at)}
        </dd>
        <dt>Run</dt>
        <dd data-testid={`tool-approval-run-${id}`}>{String(approval.run_id ?? "")}</dd>
      </dl>

      <ResourceForm
        // The id is in the title because ResourceForm derives its heading id from the title, and two calls of
        // the SAME tool parked at once is an ordinary state — two identical titles would be two identical DOM
        // ids, which is an axe violation and a lie to a screen reader about which form it is in.
        title={`Decide ${approval.identity ?? ""} (${id})`}
        testId={`tool-approval-${id}`}
        note={
          <>
            <strong>Read the arguments above, not this sentence.</strong> The decision is bound to the request
            hash shown there; if the call changed since this row was rendered, it authorizes nothing rather than
            authorizing something else.
          </>
        }
        fields={[
          {
            name: `reason-${id}`,
            label: "Reason (required to deny)",
            kind: "textarea",
            value: reason,
            onChange: setReason,
            hint: "This text is handed to the MODEL verbatim as the answer to its request, so write it for the agent: what must not happen, and what it should do instead. It is not sent on an approval.",
            testId: `tool-approval-reason-${id}`,
          },
        ]}
        submitLabel="Approve this call"
        submittingLabel="Deciding…"
        submitTestId={`tool-approval-approve-${id}`}
        submitting={busy}
        error={error}
        onSubmit={() => decide(true)}
        actions={
          <Button testId={`tool-approval-deny-${id}`} disabled={busy} onClick={() => void decide(false)}>
            Deny with this reason
          </Button>
        }
      />
    </>
  );
}
