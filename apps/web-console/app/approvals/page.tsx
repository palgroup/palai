"use client";

import { useEffect, useState } from "react";

import { ApprovalRow, type ToolApproval } from "@/components/ApprovalRow";
import { Panel } from "@/components/Panel";

// THE APPROVAL QUEUE — a screen for the surface E23 T9 opened, and the NAME of what it does not show
// (E25 T5, plan §T5).
//
// THIS TASK IS SMALL AND ITS SMALLNESS IS A MEASUREMENT (plan §3.6 D3). The feature list that sized "a console
// decision surface for tool approvals" at L was written four hours before E23 T9 landed the whole backend half:
// GET /v1/approvals plus two decision routes, a new `approve` capability that is deliberately NOT `provision`,
// the screen derived from the one shared derivation, and the request hash made mandatory at the edge. What was
// left was a page. A plan that re-plans a closed hole pays for it twice.
//
// WHAT THIS QUEUE DOES NOT COVER IS WRITTEN ON THE SCREEN (plan §3.6 D4), and it is the first thing on it.
// GET /v1/approvals carries TOOL approvals only — coordinator.PendingToolApprovals. A PUBLICATION approval (a
// push, a pull request, a merge: every side effect E22 produces) is not in this list and there is no list route
// for one at all; `API-3`/`API-4` in known-gaps-1.0.md have been filed three times and are still unapproved. So
// publication approvals remain visible only inside a live run's stream. Showing half the pending decisions
// under the heading "approvals" would be silent truncation on the one surface where it costs most — the operator
// would learn that this page is where decisions live, and then miss the ones that are not here.
//
// THE DECISION IS RECORDED AGAINST A KEY, NOT A PERSON (plan §3.6 D9, HIL-P2), and that is on the screen too.
// Every approval given here is stamped `key:<api_key_id>` server-side (coordinator.ApproverPrincipal via
// internal/store/approvals.go:99) — there is no user identity in Palai and this console has one operator
// password. "Two humans approved" is not expressible, and a screen that let someone believe otherwise would be
// manufacturing exactly the false confidence an approval gate exists to prevent.
//
// NO PUSH NOTIFICATION: THIS QUEUE POLLS. There is no SSE surface for the tool-approval queue, and adding one
// is a separate decision rather than a detail of this page. What polling buys is nevertheless the whole point
// of the task: until now a parked call could ONLY be seen inside a live stream, which meant it could only be
// seen by someone already watching. An approval that arrives while the browser is closed is here when it opens.

// POLL_MS is the refresh period, and the sentence below is derived from it so the two cannot drift. The proof
// reads it back off the DOM rather than importing it, because a claim about the timer is worth nothing if the
// number in the prose is a second copy.
//
// ponytail: setInterval + the reload signal Panel already takes. No SSE, no visibility-change tuning, no
// exponential backoff, no websocket. Upgrade path: if the control plane ever streams approval.* for tool calls
// on a per-project channel, subscribe THEN and delete the interval.
export const POLL_MS = 10_000;

export default function ApprovalsPage() {
  const [reloadKey, setReloadKey] = useState(0);
  // THE CONFIRMATION LIVES HERE, NOT IN THE ROW, and that is a repair rather than a layout choice: an applied
  // decision makes the row LEAVE the queue, so a message rendered inside the row is destroyed by the refetch
  // that proves the decision landed. The operator would click, watch the row vanish, and have nothing on screen
  // telling them what was recorded — on the one surface where "what did I just authorize?" is the whole question.
  const [decided, setDecided] = useState("");

  useEffect(() => {
    const timer = setInterval(() => setReloadKey((n) => n + 1), POLL_MS);
    return () => clearInterval(timer);
  }, []);

  return (
    <>
      <p className="muted" data-testid="approvals-scope-note">
        <strong>This queue holds tool approvals only.</strong> A gated tool call parks a run and waits here
        (<code>GET /v1/approvals</code>). <strong>Publication approvals are not here</strong>{" "}
        — a push, a pull
        request or a merge is approved inside a live run&apos;s event stream, because the public API has no list
        route for them (filed as <code>API-3</code>/<code>API-4</code> and not approved). If you are waiting to
        approve a push, watch it on <a href="/runs">Live runs</a>; an empty queue on this page does not mean
        nothing is waiting for you.{" "}
        {/* E28 T3, plan §3.6 D20. A MACHINE waiting to be admitted into a strict runner pool is a pending
            decision that is structurally not in this list, and the reason is the server's own: this queue
            reads spine.PendingToolApprovals, and the decision route requires a request_hash that a machine
            enrolment does not have. Naming it here is the same rule the sentences above already apply to
            publication approvals — a queue whose scope is unstated teaches an operator that everything
            pending is on one screen. */}
        <strong>A machine waiting to be admitted is not here either.</strong> A strict runner pool holds a new
        machine until a human admits it, and that decision cannot ride this queue: a machine enrolment has{" "}
        <strong>no request hash</strong> to bind a decision to, and everything decided here binds to one.
        Those are on <a href="/fleet">Fleet</a>.
      </p>
      {/* WHAT STAYS OPEN AND WHAT COLLAPSES IS THE DECISION, not the styling. The scope note above changes
          what an EMPTY queue means — "nothing is waiting" versus "nothing of this kind is waiting" — so it is
          read on every visit and stays. The three below are standing facts about the deployment: true on
          every visit, unchanged by anything on screen, and read once. Four paragraphs of equal weight before
          the queue is how a screen stops being read at all. They are still text, still in the DOM, still
          keyboard-reachable, and the summary says what is inside rather than "more". */}
      <details className="notes" data-testid="approvals-standing-notes">
        <summary>How a decision here is recorded, what gates it, and why this queue polls</summary>
      <p className="muted" data-testid="approvals-principal-note">
        <strong>A decision made here is recorded against the console&apos;s API key</strong> (
        <code>key:&lt;api_key_id&gt;</code>), not against a person. Palai has no user identity: a principal is an
        account on a surface. Everyone who holds this console&apos;s password decides as the same key, so
        &quot;who approved this&quot; is answerable only as far as the key.
      </p>
      <p className="muted" data-testid="approvals-gates-note">
        Two independent gates stand between a key and a decision: the key must hold the <code>approve</code>{" "}
        capability, and the project&apos;s <code>config_policy.approvers</code> must permit its principal.{" "}
        <strong>Both are permissive when unset</strong>, so an unconfigured deployment lets any of its own tenant
        keys approve. That posture is unchanged by this console — the recipe for closing both gates is in{" "}
        <code>docs/operations/console.md</code>.
      </p>
      <p className="muted" data-testid="approvals-poll-note" data-poll-ms={POLL_MS}>
        {/* One template expression rather than text around a value: JSX drops the whitespace between an
            expression at the start of a line and the text after it, which rendered "every 10seconds" and was
            caught by the spec that checks the sentence against the attribute. */}
        {`There is no push notification. This queue is re-read when the page opens, after every decision, and every ${POLL_MS / 1000} seconds — a refresh returns to the first page of the list. `}
        An approval that arrived while the browser was closed is here when you open it, which is the improvement
        over &quot;only inside a live stream&quot;; a live event surface for this queue is a separate decision.
      </p>
      </details>

      {decided === "" ? null : (
        // role="status": announced without stealing focus, which matters when the row it described has gone.
        <p role="status" className="form-status" data-testid="approvals-decision-status">
          <span className="glyph" aria-hidden="true">
            ✔
          </span>{" "}
          {decided}
        </p>
      )}

      <Panel<ToolApproval>
        title="Pending tool approvals"
        testId="panel-approvals"
        fetchPath="/approvals"
        reloadKey={reloadKey}
        note="Oldest first — the oldest question is the one closest to its deadline. Read the arguments, then decide; the decision is bound to the hash the row carries."
        // ONE COLUMN, and the row IS the record. A parked call is read, not scanned: its arguments are
        // multi-line canonical JSON and they are the authority on this screen. Panel is still what fetches,
        // renders the loading / empty / error states and — the reason it is not hand-rolled here — STATES THE
        // CUT when the server says rows remain, because this collection goes through renderPage like every
        // other and is cut at twenty (api/pagination.go defaultPageLimit).
        columns={[
          {
            header: "Parked tool call",
            render: (row) => (
              <ApprovalRow
                approval={row}
                onDecided={(outcome) => {
                  setDecided(outcome);
                  setReloadKey((n) => n + 1);
                }}
              />
            ),
          },
        ]}
      />
    </>
  );
}
