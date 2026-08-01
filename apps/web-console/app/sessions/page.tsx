"use client";

import { useMemo, useState } from "react";

import { Button } from "@/components/ui/Button";
import { LaneStrip } from "@/components/LaneStrip";
import { Panel, type Column } from "@/components/Panel";
import { Menu } from "@/components/ui/Menu";
import { Select } from "@/components/ui/Select";
import { AgentChips, CopyButton, RenameSession, SessionName, shortId, Stamp } from "@/components/Session";
import { Status, statusTone } from "@/components/Status";
import { type BadgeTone } from "@/components/ui/Badge";
import { apiSend, RelayError } from "@/lib/api";
import { compactTokens, type SessionRow } from "@/lib/sessions";
import { spanMark, windowOf } from "@/lib/strip";

// SESSIONS — the list this console did not have, over the row migration 000048 built for it.
//
// EVERY COLUMN IS A FIELD, and the mapping is the whole design: ID -> `id`, Name -> `name` + `name_source`,
// Status -> `status`, Agent -> `agents`, Tokens -> `input_tokens`/`output_tokens`, Created -> `created_at`.
// There is no column here computed from something other than the row, and no column the row cannot fill.
//
// THE THREE CONTROLS ARE NOT THE SAME KIND OF CONTROL, and the screen does not pretend otherwise:
//
//   Status and Created are SERVER filters. api/pagination.go's beginList parses ?status= (sessions is one of
//   the two kinds with a lifecycle column) and ?created_after=/?created_before= as RFC3339, so choosing one
//   re-fetches and narrows the WHOLE collection — including the rows past the twentieth this page has not
//   loaded.
//   Agent is a CLIENT filter, because there is no ?agent= to send: `agents` is an aggregate over the
//   session's runs, computed per row. It narrows what is on screen and the row count says "N of M".
//   The search box is a CLIENT filter scoped to the ID, for the same reason — /v1 mounts no ?q= anywhere.
//
// A control that silently did one while looking like the other is the defect this tree keeps finding, so the
// two server controls are the ones that reload and the two client controls are the ones inside the toolbar's
// own stated scope.

/** The lifecycle words session.json's `known_statuses` enumerates. The API's filter takes any string. */
const STATUSES = ["active", "paused", "closing", "closed", "deleted"];

/**
 * The Created windows, as an offset from the moment the operator picks one.
 *
 * THE TIMESTAMP IS COMPUTED ON CHANGE AND NOT ON RENDER, and that is load-bearing rather than tidy: the fetch
 * path is what components/Panel.tsx's effect depends on, so a `created_after` derived from Date.now() during
 * render would be a new string every render and the panel would re-fetch forever.
 */
const WINDOWS: { key: string; label: string; ms: number }[] = [
  { key: "", label: "Any time", ms: 0 },
  { key: "1h", label: "Last hour", ms: 3_600_000 },
  { key: "24h", label: "Last 24 hours", ms: 86_400_000 },
  { key: "7d", label: "Last 7 days", ms: 604_800_000 },
  { key: "30d", label: "Last 30 days", ms: 2_592_000_000 },
];

/**
 * The sentinel for "no run in this session pinned a revision" — a real thing to filter for, and not a name.
 *
 * It is worth having precisely BECAUSE the empty case is the common one: measured on the live control plane
 * 2026-07-31, `GET /v1/sessions?limit=100` answers 62 sessions and only 3 carry any agent at all. A filter
 * that could only select the 3 would be a filter for the exception.
 */
const NO_AGENT = "__no_agent__";

/**
 * TONE_LANE maps components/Status.tsx's six colour BANDS onto the lane palette, so a row's activity bar and
 * its status pill are the same colour because they came from the same decision.
 *
 * THE FIRST DRAFT OF THIS WAS A SECOND TABLE KEYED ON THE STATUS WORD, and it was wrong within one screen:
 * it called `paused` an approval (amber) while the classifier called it neutral (grey), so the same row
 * carried an amber bar and a grey pill. Two tables reading one field is the shape this tree keeps finding
 * after it ships — so the classifier is the only thing that reads a status word, and this maps its ANSWER.
 *
 * `neutral` gets no hue at all rather than a lane picked to fill the gap: a state this console has no colour
 * for should look like one, in both places.
 */
const TONE_LANE: Record<BadgeTone, { lane: string; failure?: boolean }> = {
  live: { lane: "progress" },
  info: { lane: "message" },
  warn: { lane: "approval" },
  ok: { lane: "terminal" },
  danger: { lane: "terminal", failure: true },
  neutral: { lane: "none" },
};

export default function SessionsPage() {
  const [status, setStatus] = useState("");
  const [windowKey, setWindowKey] = useState("");
  const [createdAfter, setCreatedAfter] = useState("");
  const [agent, setAgent] = useState("");
  const [rows, setRows] = useState<SessionRow[]>([]);
  const [renaming, setRenaming] = useState("");
  const [reload, setReload] = useState(0);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState("");

  const fetchPath = useMemo(() => {
    const params = new URLSearchParams();
    if (status !== "") params.set("status", status);
    if (createdAfter !== "") params.set("created_after", createdAfter);
    const query = params.toString();
    return query === "" ? "/sessions" : `/sessions?${query}`;
  }, [status, createdAfter]);

  // The agent options are the agents the LOADED rows actually name — never a list this page invented, so the
  // dropdown can never offer a value that matches nothing.
  const agentNames = useMemo(() => [...new Set(rows.flatMap((r) => r.agents ?? []))].sort(), [rows]);

  // THE ONE WINDOW EVERY ACTIVITY BAR IS DRAWN IN. It is derived from the rows this panel actually loaded —
  // not from the clock — so the leftmost bar always touches the left edge and the rightmost always touches
  // the right. A window anchored to `now` would put twenty bars in the last 3% of the track on a deployment
  // that ran yesterday, which is a chart that is technically correct and shows nothing.
  //
  // Every stamp goes in, `created_at` included: a session that never ran contributes no bar but it does
  // contribute a moment, and leaving it out would let one unrun session sit outside the window it belongs to.
  const listWindow = useMemo(
    () => windowOf(rows.flatMap((r) => [r.created_at, r.first_activity_at, r.last_activity_at])),
    [rows],
  );
  // Sessions in the loaded page whose runs pinned no agent revision. The fact the Agents column used to print
  // fifteen times, counted once — see components/Session.tsx's AgentChips for why it moved.
  const unpinned = rows.filter((r) => (r.agents ?? []).length === 0).length;

  function chooseWindow(key: string) {
    setWindowKey(key);
    const chosen = WINDOWS.find((w) => w.key === key);
    setCreatedAfter(chosen === undefined || chosen.ms === 0 ? "" : new Date(Date.now() - chosen.ms).toISOString());
  }

  async function createSession() {
    setCreating(true);
    setError("");
    try {
      await apiSend<SessionRow>("POST", "/sessions", {});
      // Back to page one: the new session is the newest row, and the list orders newest first.
      setReload((n) => n + 1);
    } catch (err: unknown) {
      setError(err instanceof RelayError ? err.problem.detail : "the session could not be created");
    } finally {
      setCreating(false);
    }
  }

  const columns: Column<SessionRow>[] = [
    {
      header: "ID",
      sort: (r) => r.id,
      // A SHORT FORM PLUS A COPY BUTTON, which is the reference's measured id cell and a correction to what
      // this console does everywhere else: a raw 36-character id sitting in a column, which an operator can
      // neither read nor reliably select. The short form is DERIVED from the id (shortId), the whole value
      // is in `title` and on the clipboard, and the link is the way in — so nothing is hidden and nothing
      // has to be retyped.
      render: (r) => (
        <span className="cell-id-group">
          <a className="cell-id" href={`/sessions/${encodeURIComponent(r.id)}`} title={r.id} data-testid="session-link">
            {shortId(r.id)}
          </a>
          <CopyButton value={r.id} label="session ID" testId="session-copy-id" />
        </span>
      ),
    },
    {
      header: "Name",
      sort: (r) => r.name,
      // THE NAME IS A LINK TOO — measured: "ID ve Name, ikisi de detaya link — kullanıcı nereye tıklarsa
      // tıklasın gider". A row where only the id is clickable makes the operator aim at the one cell they
      // cannot read.
      render: (r) =>
        renaming === r.id ? (
          <RenameSession
            sessionId={r.id}
            name={r.name}
            testId="session-rename"
            onCancel={() => setRenaming("")}
            onDone={() => {
              setRenaming("");
              setReload((n) => n + 1);
            }}
          />
        ) : (
          <a className="cell-name-link" href={`/sessions/${encodeURIComponent(r.id)}`} data-testid="session-name-link">
            <SessionName name={r.name} source={r.name_source} />
          </a>
        ),
    },
    { header: "Status", sort: (r) => r.status, render: (r) => <Status value={r.status} testId="session-status" /> },
    // "Agents", PLURAL, and session.json spells out why in its own words: "a screen that labels this column
    // 'Agent' singular is asserting something the data does not say". There is no session-to-agent
    // association in this system — a RUN pins a revision — so a session may have used none, one, or several,
    // and the fixture's four-agent row is not a special case, it is the shape.
    //
    // NO SORT, and Column's own contract says why: `agents` is a SET, and a set has no order that means
    // anything. Sorting a list of sessions by the alphabetical first of their agents is a control that
    // produces motion and no information.
    { header: "Agents", render: (r) => <AgentChips agents={r.agents ?? []} /> },
    {
      header: "Tokens in / out",
      sort: (r) => r.input_tokens,
      numeric: true,
      render: (r) => (
        <span className="num" data-testid="session-tokens">
          {compactTokens(r.input_tokens)} / {compactTokens(r.output_tokens)}
        </span>
      ),
    },
    { header: "Created", sort: (r) => r.created_at, render: (r) => <Stamp iso={r.created_at} /> },
    // THE LANE STRIP AT ROW SCALE, AND IT IS A SPAN RATHER THAN LANES — the honest reduction, recorded in
    // app/globals.css §11 and worth repeating where a reader meets it. GET /v1/sessions carries no events, so
    // lanes here would mean twenty journal streams; what the response DOES carry is first_activity_at and
    // last_activity_at, and those two placed inside the window the whole list covers turn this column into a
    // chart of the deployment's day. Sorted newest-first as the API serves them, the bars step across the
    // track — and a session that ran for an hour is instantly a different object from one that ran for a
    // second, which no pair of timestamps in a cell has ever made visible.
    //
    // NOT SORTABLE, for Column's own stated reason: the value here is a POSITION IN A SHARED WINDOW, and
    // re-ordering the rows changes what the column means rather than what it shows. Created sorts; this reads.
    {
      header: "Activity",
      render: (r) => {
        const mark = spanMark(r.first_activity_at, r.last_activity_at, listWindow);
        if (mark === null) {
          return (
            <span className="cell-none" title="This session has no first_activity_at — no run of it has ever started.">
              —
            </span>
          );
        }
        const tone = TONE_LANE[statusTone(r.status)];
        return (
          <LaneStrip
            scale="span"
            testId="session-activity"
            channels={[{ lane: tone.lane, label: "Activity", marks: [{ ...mark, failure: tone.failure }] }]}
          />
        );
      },
    },
    {
      header: "",
      render: (r) => (
        <div className="row-menu">
          <Menu
            label={`Actions for session ${r.id}`}
            trigger={<span aria-hidden="true">⋯</span>}
            triggerClassName="row-menu-toggle"
            triggerTestId="session-menu"
            popupTestId="session-menu-panel"
            // ONE ITEM, AND THAT IS THE API RATHER THAN THE DESIGN. A session's whole write surface is
            // PATCH /v1/sessions/{id} (api/router.go:324) — there is no close, no delete, no archive and
            // no re-run — so a second entry here would have to be a control that refuses.
            items={[{ label: "Rename", testId: "session-rename-open", onSelect: () => setRenaming(r.id) }]}
          />
        </div>
      ),
    },
  ];

  return (
    <>
      {error === "" ? null : (
        <p role="alert" className="form-error" data-testid="session-create-error">
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {error}
        </p>
      )}

      <Panel<SessionRow>
        title="Sessions"
        testId="panel-sessions"
        fetchPath={fetchPath}
        reloadKey={reload}
        columns={columns}
        onRows={setRows}
        narrow={(r) => (agent === "" ? true : agent === NO_AGENT ? (r.agents ?? []).length === 0 : (r.agents ?? []).includes(agent))}
        matchOn={(r) => r.id}
        filterLabel="Search sessions by ID"
        filterPlaceholder="Session ID…"
        footnote={
          unpinned === 0 ? undefined : (
            <>
              {unpinned} of the {rows.length} sessions loaded pinned no agent revision, so their Agents cell is
              empty. A run pins a REVISION — choosing an agent without one pins nothing, and there is then
              nothing for this column to aggregate.
            </>
          )
        }
        action={
          <Button variant="primary" onClick={() => void createSession()} disabled={creating} testId="session-create">
            {creating ? "…" : "+ New session"}
          </Button>
        }
        tools={
          <>
            <Select
              label="Status"
              testId="session-status-filter"
              value={status}
              onValueChange={setStatus}
              options={[{ value: "", label: "Any status" }, ...STATUSES.map((s) => ({ value: s, label: s }))]}
            />
            <Select
              label="Created"
              testId="session-created-filter"
              value={windowKey}
              onValueChange={chooseWindow}
              options={WINDOWS.map((w) => ({ value: w.key, label: w.label }))}
            />
            <Select
              label="Agents"
              testId="session-agent-filter"
              value={agent}
              onValueChange={setAgent}
              options={[
                { value: "", label: "Any agent" },
                // The row's words are the CELL's words, because they select the same rows. "No agent" would
                // name a state this system does not have: what a run carries is a REVISION pin, and its
                // absence is what leaves this column empty on 59 of the 62 sessions the live stack holds.
                { value: NO_AGENT, label: "No revision pinned" },
                ...agentNames.map((name) => ({ value: name, label: name })),
              ]}
            />
          </>
        }
        emptyNote={
          // THE MEASURED SHAPE (design-reference/screen-inventory.md §3): a title, then one sentence saying
          // what the thing IS — "No memory stores yet / Memory stores give agents persistent, cross-session
          // memory". An empty screen is the one moment a reader can be taught, and `None yet.` teaches
          // nothing. This console still says those three words on three other panels; they are the
          // tree-wide pass's, and these two screens do not add a fourth.
          <>
            <p className="empty-title" data-testid="session-empty-title">
              No sessions yet
            </p>
            <p className="empty-body">
              A session is one conversation with this deployment — every run it holds, the events they
              journal and the tokens they spend belong to it.
            </p>
            <Button variant="primary" onClick={() => void createSession()} disabled={creating} testId="session-create-empty">
              Create one
            </Button>
          </>
        }
      />
    </>
  );
}
