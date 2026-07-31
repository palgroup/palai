"use client";

import { useMemo, useState } from "react";

import { Panel, type Column } from "@/components/Panel";
import { AgentChips, RenameSession, SessionName, Stamp } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiSend, RelayError } from "@/lib/api";
import { compactTokens, type SessionRow } from "@/lib/sessions";

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

/** The sentinel for "a session no run pinned an agent to" — a real thing to filter for, and not a name. */
const NO_AGENT = "__no_agent__";

export default function SessionsPage() {
  const [status, setStatus] = useState("");
  const [windowKey, setWindowKey] = useState("");
  const [createdAfter, setCreatedAfter] = useState("");
  const [agent, setAgent] = useState("");
  const [rows, setRows] = useState<SessionRow[]>([]);
  const [renaming, setRenaming] = useState("");
  const [menu, setMenu] = useState("");
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
      // THE ID IS THE LINK, which is why it carries no separate "open" control: the identity of the row and
      // the way into it are the same thing. It is clipped by CSS with the full value in `title`, so the
      // clipping is recoverable rather than silent — and it is an <a>, so it is in the tab order already.
      render: (r) => (
        <a className="cell-id" href={`/sessions/${encodeURIComponent(r.id)}`} title={r.id} data-testid="session-link">
          {r.id}
        </a>
      ),
    },
    {
      header: "Name",
      sort: (r) => r.name,
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
          <SessionName name={r.name} source={r.name_source} />
        ),
    },
    { header: "Status", sort: (r) => r.status, render: (r) => <Status value={r.status} testId="session-status" /> },
    // NO SORT, and Column's own contract says why: `agents` is a SET, and a set has no order that means
    // anything. Sorting a list of sessions by the alphabetical first of their agents is a control that
    // produces motion and no information.
    { header: "Agent", render: (r) => <AgentChips agents={r.agents ?? []} /> },
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
    {
      header: "",
      render: (r) => (
        <div className="row-menu">
          <button
            type="button"
            className="row-menu-toggle"
            aria-expanded={menu === r.id}
            aria-controls={`menu-${r.id}`}
            aria-label={`Actions for session ${r.id}`}
            data-testid="session-menu"
            onClick={() => setMenu(menu === r.id ? "" : r.id)}
            onKeyDown={(e) => {
              if (e.key === "Escape") setMenu("");
            }}
          >
            <span aria-hidden="true">⋯</span>
          </button>
          {menu === r.id ? (
            <div className="row-menu-panel" id={`menu-${r.id}`}>
              {/* ONE ITEM, AND THAT IS THE API RATHER THAN THE DESIGN. A session's whole write surface is
                  PATCH /v1/sessions/{id} (api/router.go:324) — there is no close, no delete, no archive and
                  no re-run — so a second entry here would have to be a control that refuses. */}
              <button
                type="button"
                data-testid="session-rename-open"
                onClick={() => {
                  setRenaming(r.id);
                  setMenu("");
                }}
              >
                Rename
              </button>
            </div>
          ) : null}
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
        action={
          <button type="button" className="primary" onClick={() => void createSession()} disabled={creating} data-testid="session-create">
            {creating ? "…" : "+ New session"}
          </button>
        }
        tools={
          <>
            <select aria-label="Status" data-testid="session-status-filter" value={status} onChange={(e) => setStatus(e.target.value)}>
              <option value="">Any status</option>
              {STATUSES.map((s) => (
                <option key={s} value={s}>
                  {s}
                </option>
              ))}
            </select>
            <select aria-label="Created" data-testid="session-created-filter" value={windowKey} onChange={(e) => chooseWindow(e.target.value)}>
              {WINDOWS.map((w) => (
                <option key={w.key} value={w.key}>
                  {w.label}
                </option>
              ))}
            </select>
            <select aria-label="Agent" data-testid="session-agent-filter" value={agent} onChange={(e) => setAgent(e.target.value)}>
              <option value="">Any agent</option>
              <option value={NO_AGENT}>No agent pinned</option>
              {agentNames.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </select>
          </>
        }
        emptyNote={
          <>
            No session matches, so there is nothing to open.{" "}
            <button type="button" className="primary" onClick={() => void createSession()} disabled={creating} data-testid="session-create-empty">
              Create one
            </button>
          </>
        }
      />
    </>
  );
}
