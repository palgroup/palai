"use client";

import { usePathname, useRouter, useSearchParams } from "next/navigation";
import { Suspense, useEffect, useState } from "react";

import { ApprovalRow, type ToolApproval } from "@/components/ApprovalRow";
import { Panel, type Column } from "@/components/Panel";
import { CopyButton, shortId, Stamp } from "@/components/Session";

// THE APPROVAL QUEUE — a screen for the surface E23 T9 opened, and the NAME of what it does not show
// (E25 T5, plan §T5).
//
// WHAT THE PAGE-PARITY PASS FOUND HERE, and it was the worst screen in the console by every measure taken
// (built console, 2026-07-31): 7529 characters of <main>, 3626 of them grey prose, **seven forms**, ONE table
// column, and eight <h2>s — because every parked call rendered its whole ledger `<dl>` AND a full decision
// form, inline, in a single-column table. Seven questions asked at once, each with its own textarea, is a
// screen an operator cannot triage: the thing a queue is FOR — seeing what is waiting — was the one thing it
// could not do.
//
// SO THE QUEUE IS A TABLE AND THE DECISION IS ONE PANEL BELOW IT, for the row the operator selected. The
// selection is in the URL (`?approval=…`), which is the pattern app/sessions/[id]/page.tsx landed: the back
// button undoes a selection, a parked call can be sent to a colleague as a link, and a reload lands on the
// call that was being read.
//
// IT IS NOT A `/approvals/[id]` ROUTE, AND THAT IS A MEASUREMENT RATHER THAN A PREFERENCE. lib/routes.ts's
// DynamicConsoleRoute requires a way to MAKE the path concrete — the first row of a collection, or a POST when
// that collection is empty — precisely so an empty deployment cannot silently leave a screen unscanned. There
// is no `GET /v1/approvals/{id}` (api/router.go:295-297 mounts a list and two decision routes) and **no /v1
// route can park an approval at all**: a tool approval exists only when a gated tool call parks, which is what
// DIV-UI-006 records and why a compose stack's queue is permanently empty. A declared dynamic route here would
// therefore throw on the real profile, and the alternative — letting it skip — is the silent-skip trap that
// file exists to abolish. A pane on a scanned route is covered by the scan the route already has.
//
// WHAT THIS QUEUE DOES NOT COVER IS STILL ON THE SCREEN (plan §3.6 D4) and is the one paragraph that stayed.
// It is not decoration: it changes what an EMPTY queue MEANS. GET /v1/approvals carries TOOL approvals only —
// a publication approval (a push, a pull request, a merge) has no list route at all and lives inside a live
// run's stream, and a machine waiting for admission cannot ride this queue because a machine enrolment has no
// request hash to bind a decision to. An operator who learns "approvals live here" would miss both.
//
// THE THREE STANDING FACTS — the principal a decision is recorded against, the two gates in front of it, and
// the polling period — are in the collapsed <details> they were already in, and they stay ON THIS PAGE rather
// than moving to the decision panel. That is forced rather than chosen: the real profile's queue is EMPTY, so
// a sentence that only renders beside a selected row is a sentence an operator on a real stack can never read,
// and it is the one profile where those three facts matter most.

// POLL_MS is the refresh period, and the sentence below is derived from it so the two cannot drift. The proof
// reads it back off the DOM rather than importing it, because a claim about the timer is worth nothing if the
// number in the prose is a second copy.
//
// ponytail: setInterval + the reload signal Panel already takes. No SSE, no visibility-change tuning, no
// exponential backoff, no websocket. Upgrade path: if the control plane ever streams approval.* for tool calls
// on a per-project channel, subscribe THEN and delete the interval.
export const POLL_MS = 10_000;

// THE SUSPENSE BOUNDARY IS REQUIRED, AND IT IS A BUILD ERROR RATHER THAN A PREFERENCE. `useSearchParams()`
// forces a client bail-out during prerendering, and Next refuses to build a STATIC page that calls it outside
// one: "useSearchParams() should be wrapped in a suspense boundary at page /approvals". The two screens that
// already put state in the URL are both DYNAMIC routes (`/sessions/[id]`, `/agents/[id]`), which is why this
// is the first place in the console to meet it — a `[id]` segment makes the page server-rendered on demand
// and there is no prerender to bail out of.
//
// The fallback is what a reader sees for the instant before hydration, and it is deliberately NOT the panel's
// name or its spinner: `panel-approvals` is this route's declared readiness signal (lib/routes.ts), so a
// fallback answering to it would let an axe scan analyse a placeholder and call the page clean.
export default function ApprovalsPage() {
  return (
    <Suspense fallback={<p className="loading">Loading…</p>}>
      <ApprovalQueue />
    </Suspense>
  );
}

function ApprovalQueue() {
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  // THE URL IS THE SOURCE, not a mirror: `selected` is read out of it on every render rather than being state
  // that is also written to the address bar, so the two can never disagree.
  const selected = search.get("approval") ?? "";

  const [rows, setRows] = useState<ToolApproval[]>([]);
  const [reloadKey, setReloadKey] = useState(0);
  const [decided, setDecided] = useState("");

  useEffect(() => {
    const timer = setInterval(() => setReloadKey((n) => n + 1), POLL_MS);
    return () => clearInterval(timer);
  }, []);

  function choose(id: string) {
    const params = new URLSearchParams(search.toString());
    // A parameter set to nothing is DELETED rather than emptied — `?approval=` is a URL that says a call is
    // selected and does not say which.
    if (id === "") params.delete("approval");
    else params.set("approval", id);
    const query = params.toString();
    // scroll:false — selecting a call must not jump the reader to the top of the queue.
    router.push(query === "" ? pathname : `${pathname}?${query}`, { scroll: false });
  }

  // THE SELECTED ROW IS FOUND IN THE LIST THIS PAGE JUST READ, never fetched on its own — there is no route
  // that would answer one. A `?approval=` naming a row the queue no longer holds therefore renders the "it is
  // not here any more" state below rather than a spinner, and that is the honest reading: an answered row, a
  // reaped row and another project's row are deliberately indistinguishable on this surface.
  const chosen = rows.find((r) => String(r.id ?? "") === selected) ?? null;

  const columns: Column<ToolApproval>[] = [
    {
      header: "ID",
      sort: (r) => String(r.id ?? ""),
      render: (r) => (
        <span className="cell-id-group">
          <code className="cell-id">{shortId(String(r.id ?? ""))}</code>
          <CopyButton value={String(r.id ?? "")} label="approval ID" testId="approval-copy-id" />
        </span>
      ),
    },
    {
      // THE TOOL IDENTITY IS THE ROW'S NAME and it is the control that opens it. NOT a link and not
      // aria-selected: a <tr> is role="row", and aria-selected is only allowed on a row inside a grid or
      // treegrid — axe's aria-allowed-attr rule (wcag2a) refuses it on a plain table. The selection is stated
      // where it belongs, as aria-pressed on the control that toggles it.
      header: "Tool",
      sort: (r) => String(r.identity ?? ""),
      render: (r) => {
        const id = String(r.id ?? "");
        return (
          <button
            type="button"
            className="row-select"
            aria-pressed={selected === id}
            data-testid="approval-open"
            data-approval-id={id}
            onClick={() => choose(selected === id ? "" : id)}
          >
            <code>{String(r.identity ?? "")}</code>
          </button>
        );
      },
    },
    {
      // WHAT A HUMAN WROTE ABOUT THIS TOOL AT REGISTRATION — the only sentence on this surface whose author
      // has no interest in the answer. There is no MCP `description` on api.PendingApproval at all.
      header: "Operator label",
      render: (r) =>
        String(r.operator_label ?? "").trim() === "" ? (
          <span className="cell-none">— none written</span>
        ) : (
          String(r.operator_label)
        ),
    },
    { header: "Run", render: (r) => <code>{shortId(String(r.run_id ?? ""))}</code> },
    {
      // A DEADLINE IN THE FUTURE, rendered relatively, which is what an operator triaging a queue actually
      // reads: "in 4 minutes" is the question's urgency and "2026-07-30T12:25:00Z" is not. The absolute stamp
      // is the title and the datetime, and the decision panel below shows it in full.
      header: "Deadline",
      sort: (r) => String(r.expires_at ?? ""),
      render: (r) =>
        r.expires_at === undefined ? (
          <span className="cell-none" title="This gate has no deadline, so the run waits until somebody answers.">
            — none
          </span>
        ) : (
          <Stamp iso={String(r.expires_at)} />
        ),
    },
  ];

  return (
    <>
      {/* THE ONE PARAGRAPH THAT STAYED, and every clause in it changes what an EMPTY queue MEANS. The explicit
          {" "} after each closing tag is not decoration: the JSX transform trims the leading whitespace off a
          text child that begins on the line after a closing tag, which shipped "not here— a push" the first
          time this sentence was rewritten and was caught by the spec on the next run. */}
      <p className="muted" data-testid="approvals-scope-note">
        This queue holds <strong>tool approvals only</strong>.{" "}
        <strong>Publication approvals are not here</strong>{" "}
        — a push, a pull request or a merge is approved inside a live run&apos;s stream, because the public API
        has no list route for one; watch it on <a href="/runs">Live runs</a>.{" "}
        <strong>A machine waiting to be admitted is not here either</strong>: an enrolment has no{" "}
        <strong>request hash</strong> to bind a decision to, and everything decided here binds to one — those
        are on <a href="/fleet">Fleet</a>. So an empty queue{" "}
        <strong>does not mean nothing is waiting</strong> for you.
      </p>
      {/* WHAT STAYS OPEN AND WHAT COLLAPSES IS THE DECISION, not the styling. The scope note above changes
          what an EMPTY queue means, so it is read on every visit and stays. The three below are standing facts
          about the deployment: true on every visit, unchanged by anything on screen, and read once. They are
          still text, still in the DOM, still keyboard-reachable, and the summary says what is inside rather
          than "more". */}
      <details className="notes" data-testid="approvals-standing-notes">
        <summary>How a decision here is recorded, what gates it, and why this queue polls</summary>
        <p className="muted" data-testid="approvals-principal-note">
          <strong>A decision made here is recorded against the console&apos;s API key</strong> (
          <code>key:&lt;api_key_id&gt;</code>), not against a person. Palai has no user identity: a principal is
          an account on a surface. Everyone who holds this console&apos;s password decides as the same key, so
          &quot;who approved this&quot; is answerable only as far as the key.
        </p>
        <p className="muted" data-testid="approvals-gates-note">
          Two independent gates stand between a key and a decision: the key must hold the <code>approve</code>{" "}
          capability, and the project&apos;s <code>config_policy.approvers</code> must permit its principal.{" "}
          <strong>Both are permissive when unset</strong>, so an unconfigured deployment lets any of its own
          tenant keys approve. That posture is unchanged by this console — the recipe for closing both gates is
          in <code>docs/operations/console.md</code>.
        </p>
        <p className="muted" data-testid="approvals-poll-note" data-poll-ms={POLL_MS}>
          {/* One template expression rather than text around a value: JSX drops the whitespace between an
              expression at the start of a line and the text after it, which rendered "every 10seconds" and was
              caught by the spec that checks the sentence against the attribute. */}
          {`There is no push notification. This queue is re-read when the page opens, after every decision, and every ${POLL_MS / 1000} seconds — a refresh returns to the first page of the list. `}
          An approval that arrived while the browser was closed is here when you open it, which is the
          improvement over &quot;only inside a live stream&quot;; a live event surface for this queue is a
          separate decision.
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
        onRows={setRows}
        columns={columns}
        matchOn={(r) => `${String(r.id ?? "")} ${String(r.identity ?? "")} ${String(r.operator_label ?? "")}`}
        filterLabel="Search parked calls by tool or ID"
        filterPlaceholder="Tool or ID…"
        emptyNote={
          <>
            <p className="empty-title" data-testid="approvals-empty-title">
              No tool calls are waiting
            </p>
            <p className="empty-body">
              A gated tool call parks its run here until a human answers it. Read the scope note above before
              concluding nothing needs you — this queue holds one kind of pending decision.
            </p>
          </>
        }
      />

      {/* THE DECISION, FOR ONE CALL. Below the table rather than beside it, and that is the arguments' doing:
          they are multi-line canonical JSON and the authority on this screen, so they get the page's width.
          The transcript's side pane holds a six-field frame; this holds a patch. */}
      <section className="panel" data-testid="approval-decision" aria-labelledby="approval-decision-h">
        <div className="panel-head">
          {/* The region's name, not the form's. components/ApprovalRow.tsx renders its own heading carrying the
              tool identity and the approval id, which is what a reader needs to know they are answering the
              call they clicked; repeating it here would be two headings saying one thing. */}
          <h2 id="approval-decision-h">{chosen === null ? "No call selected" : "Selected call"}</h2>
          {chosen === null ? null : (
            <button type="button" className="detail-close" data-testid="approval-close" onClick={() => choose("")}>
              Close
              <span className="sr-only"> the selected call</span>
            </button>
          )}
        </div>
        {chosen === null ? (
          <div className="empty" data-testid="approval-none-selected">
            <p className="empty-title">{selected === "" ? "No call selected" : "That call is no longer in the queue"}</p>
            <p className="empty-body">
              {selected === ""
                ? "Choose a parked call above to read exactly what will be sent, and answer it. The decision binds to the request hash that row carries and to nothing else."
                : "It was answered, its deadline passed, or it belongs to another project — this surface makes those deliberately indistinguishable. Choose another call above."}
            </p>
          </div>
        ) : (
          <ApprovalRow
            approval={chosen}
            onDecided={(outcome) => {
              setDecided(outcome);
              // The answered call has LEFT the queue, so the selection has to go with it — a `?approval=` left
              // pointing at a decided row would reopen this panel on its own "no longer here" state after
              // every reload.
              choose("");
              setReloadKey((n) => n + 1);
            }}
          />
        )}
      </section>
    </>
  );
}
