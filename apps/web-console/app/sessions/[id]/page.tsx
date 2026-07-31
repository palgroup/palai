"use client";

import { useParams } from "next/navigation";
import { Fragment, useEffect, useMemo, useState } from "react";

import { AgentChips, RenameSession, SessionName, Stamp } from "@/components/Session";
import { Status } from "@/components/Status";
import { apiGet, readSessionEvents, RelayError } from "@/lib/api";
import {
  absoluteTime,
  compactTokens,
  elapsedStamp,
  eventState,
  eventSubject,
  formatDuration,
  isFailureEvent,
  LANE_LABEL,
  positionOf,
  spanOf,
  type JournalEvent,
  type SessionRow,
} from "@/lib/sessions";
import { isTerminal, laneFor } from "@/lib/timeline";

// THE SESSION TRANSCRIPT — one session, its own journal, replayed from the record.
//
// IT IS NOT A CHAT AND IT DOES NOT PRETEND TO BE ONE. The reference this screen was drawn from shows rows
// with a User / Tool / Agent badge and a content preview, and this deployment journals neither. Measured
// against the live control plane on 2026-07-31 (`GET /v1/sessions/{id}/events`, six frames):
//
//   run.queued.v1          data: {run_id, state}
//   run.provisioning.v1    data: {run_id, state}
//   run.running.v1         data: {run_id, state}
//   model_step.created.v1  data: {model_request_id, run_id}
//   model_step.completed.v1 data: {model_request_id, run_id}
//   run.completed.v1       data: {run_id, state}
//
// No author. No addressee. No text. That is a RUN LIFECYCLE, and the honest rendering of it is the §47.2
// lane the type already belongs to — lib/timeline's laneFor, the same function the live relay projects with,
// so a replayed journal and a live stream are sorted by one classifier rather than two. Every row below is a
// field the frame carries; the parts of the reference with nothing behind them are ABSENT rather than
// invented, and the report that shipped with this screen lists them.

type Tab = "transcript" | "debug";
const TABS: { id: Tab; label: string }[] = [
  { id: "transcript", label: "Transcript" },
  { id: "debug", label: "Debug" },
];

export default function SessionTranscriptPage() {
  const params = useParams<{ id: string }>();
  const sessionID = Array.isArray(params.id) ? (params.id[0] ?? "") : (params.id ?? "");

  const [session, setSession] = useState<SessionRow | null>(null);
  const [sessionError, setSessionError] = useState("");
  const [events, setEvents] = useState<JournalEvent[]>([]);
  const [journalError, setJournalError] = useState("");
  const [tab, setTab] = useState<Tab>("transcript");
  const [typeFilter, setTypeFilter] = useState("");
  const [query, setQuery] = useState("");
  const [selected, setSelected] = useState<number | null>(null);
  const [view, setView] = useState<"rendered" | "raw">("rendered");
  const [renaming, setRenaming] = useState(false);

  useEffect(() => {
    if (sessionID === "") return;
    let live = true;
    setSessionError("");
    apiGet<SessionRow>(`/sessions/${encodeURIComponent(sessionID)}`)
      .then((row) => {
        if (live) setSession(row);
      })
      .catch((err: unknown) => {
        // The SERVER's sentence. A 404 for an id that was never here and a 401 for a closed door are
        // different facts, and "the session could not be loaded" is neither of them.
        if (live) setSessionError(err instanceof RelayError ? err.problem.detail : "the session could not be read");
      });
    return () => {
      live = false;
    };
  }, [sessionID]);

  useEffect(() => {
    if (sessionID === "") return;
    setEvents([]);
    setJournalError("");
    const controller = new AbortController();
    readSessionEvents(
      sessionID,
      (event) => {
        const type = String(event.type ?? "");
        if (type === "") return;
        setEvents((prev) => [...prev, { ...event, type, sequence: Number(event.sequence ?? 0) }]);
        // A FINISHED session's stream closes itself (api/events.go's pump returns after a terminal frame).
        // This aborts anyway, for the session whose run is still going: this screen is the record, and a live
        // run would otherwise tail it with heartbeats for as long as the tab stayed open.
        if (isTerminal(type)) controller.abort();
      },
      controller.signal,
    ).catch((err: unknown) => {
      if (controller.signal.aborted) return;
      setJournalError(err instanceof RelayError ? err.problem.detail : "the event journal could not be read");
    });
    return () => {
      controller.abort();
    };
  }, [sessionID]);

  const ordered = useMemo(() => [...events].sort((a, b) => a.sequence - b.sequence), [events]);
  const span = useMemo(() => spanOf(ordered), [ordered]);
  const types = useMemo(() => [...new Set(ordered.map((e) => e.type))].sort(), [ordered]);
  const shown = useMemo(() => {
    const q = query.trim().toLowerCase();
    return ordered.filter((e) => {
      if (typeFilter !== "" && e.type !== typeFilter) return false;
      if (q === "") return true;
      return `${e.type} ${JSON.stringify(e.data ?? {})}`.toLowerCase().includes(q);
    });
  }, [ordered, typeFilter, query]);
  const chosen = selected === null ? null : (ordered.find((e) => e.sequence === selected) ?? null);

  function moveTab(from: Tab, delta: number) {
    const index = TABS.findIndex((t) => t.id === from);
    const next = TABS[(index + delta + TABS.length) % TABS.length];
    setTab(next.id);
    document.getElementById(`tab-${next.id}`)?.focus();
  }

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>
            <a href="/sessions">Sessions</a>
          </li>
          <li aria-current="page">
            <code data-testid="breadcrumb-session">{sessionID}</code>
          </li>
        </ol>
      </nav>

      {sessionError === "" ? null : (
        <p role="alert" className="form-error" data-testid="session-error">
          <span className="glyph" aria-hidden="true">
            ✖︎
          </span>{" "}
          {sessionError}
        </p>
      )}

      <div className="session-head">
        <h1 className="page-title" data-testid="session-title">
          {session === null ? sessionID : <SessionName name={session.name} source={session.name_source} />}
        </h1>
        {session === null || renaming ? null : (
          <button type="button" data-testid="session-rename-open" onClick={() => setRenaming(true)}>
            Rename
          </button>
        )}
      </div>

      {session !== null && renaming ? (
        <RenameSession
          sessionId={session.id}
          name={session.name}
          testId="session-rename"
          onCancel={() => setRenaming(false)}
          onDone={(row) => {
            setSession(row);
            setRenaming(false);
          }}
        />
      ) : null}

      {/* THE CHIP ROW — the session's facts, beside its name, each one wearing the name of the field it is.
          A bare "19.9s" next to a bare "22.5k / 111" is two numbers nobody can attribute; the micro-label is
          what makes a chip a reading rather than a decoration. There is no ENVIRONMENT chip, and its absence
          is the honest one: the session projection carries no environment and nothing in /v1 associates the
          two, so a chip here would be a field this console invented. */}
      {session === null ? (
        <p className="loading">Loading…</p>
      ) : (
        <ul className="chip-row" data-testid="session-chips">
          <li>
            <Status value={session.status} testId="chip-status" />
          </li>
          <li>
            <span className="chip" data-testid="chip-agent">
              <span className="chip-key">Agents</span>{" "}
              <AgentChips agents={session.agents ?? []} />
            </span>
          </li>
          <li>
            <span className="chip" data-testid="chip-duration">
              <span className="chip-key">Duration</span>{" "}
              {formatDuration(session.duration_ms)}
            </span>
          </li>
          <li>
            <span className="chip" data-testid="chip-tokens">
              <span className="chip-key">Tokens</span>{" "}
              <span className="num">
                {compactTokens(session.input_tokens)} / {compactTokens(session.output_tokens)}
              </span>
            </span>
          </li>
          <li>
            <span className="chip" data-testid="chip-created">
              <span className="chip-key">Created</span>{" "}
              <Stamp iso={session.created_at} />
            </span>
          </li>
        </ul>
      )}

      <div className="tabs" role="tablist" aria-label="Session views">
        {TABS.map((t) => (
          <button
            key={t.id}
            id={`tab-${t.id}`}
            role="tab"
            type="button"
            className="tab"
            aria-selected={tab === t.id}
            aria-controls={`panel-${t.id}`}
            tabIndex={tab === t.id ? 0 : -1}
            data-testid={`tab-${t.id}`}
            onClick={() => setTab(t.id)}
            onKeyDown={(e) => {
              if (e.key === "ArrowRight") moveTab(t.id, 1);
              if (e.key === "ArrowLeft") moveTab(t.id, -1);
            }}
          >
            {t.label}
          </button>
        ))}
      </div>

      <div role="tabpanel" id="panel-transcript" aria-labelledby="tab-transcript" tabIndex={0} hidden={tab !== "transcript"}>
        {journalError === "" ? null : (
          <p role="alert" className="form-error" data-testid="journal-error">
            <span className="glyph" aria-hidden="true">
              ✖︎
            </span>{" "}
            {journalError}
          </p>
        )}

        <div className="transcript-split">
          <section className="panel" data-testid="session-transcript" aria-labelledby="transcript-h">
            <div className="panel-head">
              <h2 id="transcript-h">Transcript</h2>
              <span className="panel-count" data-testid="transcript-count">
                {shown.length === ordered.length
                  ? `${String(ordered.length)} ${ordered.length === 1 ? "event" : "events"}`
                  : `${String(shown.length)} of ${String(ordered.length)} events`}
              </span>
              <div className="panel-tools">
                <select aria-label="Event type" data-testid="transcript-type" value={typeFilter} onChange={(e) => setTypeFilter(e.target.value)}>
                  <option value="">All event types</option>
                  {types.map((t) => (
                    <option key={t} value={t}>
                      {t}
                    </option>
                  ))}
                </select>
                <input
                  type="search"
                  aria-label="Search events"
                  placeholder="Search events…"
                  data-testid="transcript-search"
                  value={query}
                  onChange={(e) => setQuery(e.target.value)}
                />
              </div>
            </div>

            {/* THE SCRUBBER. Marks placed by each frame's own timestamp within the journal's span, so a burst
                of five frames in one second and a four-minute wait for a model read like the two different
                things they are — which a numbered list cannot show.

                IT IS aria-hidden AND ITS CAPTION IS NOT, deliberately. A track of six absolutely-positioned
                marks is not a control and carries no text; announcing it would read out six empty elements.
                The information it carries visually is in the caption in words AND in the Elapsed column of
                every row below, so nothing is available only to the eye. */}
            {span === null ? null : (
              <figure className="scrubber" data-testid="transcript-scrubber">
                <div className="scrubber-track" aria-hidden="true">
                  {ordered.map((e) => (
                    <span
                      key={e.sequence}
                      className="scrubber-mark"
                      data-lane={laneFor(e.type)}
                      data-failure={isFailureEvent(e.type) ? "true" : undefined}
                      style={{ left: `${String(positionOf(String(e.time ?? ""), span))}%` }}
                    />
                  ))}
                </div>
                <figcaption data-testid="scrubber-caption">
                  {ordered.length} {ordered.length === 1 ? "event" : "events"} over {formatDuration(span.end - span.start)}, ending at{" "}
                  {elapsedStamp(span.end - span.start)}
                </figcaption>
              </figure>
            )}

            {ordered.length === 0 ? (
              <p className="empty" data-testid="transcript-empty">
                This session has no journal yet, so there is nothing to replay.
              </p>
            ) : shown.length === 0 ? (
              <p className="empty" data-testid="transcript-no-match">
                No event here matches the filters above.
              </p>
            ) : (
              <table>
                <thead>
                  <tr>
                    <th scope="col">Lane</th>
                    <th scope="col">Event</th>
                    <th scope="col">Detail</th>
                    <th scope="col">Since previous</th>
                    <th scope="col">Elapsed</th>
                  </tr>
                </thead>
                <tbody>
                  {shown.map((e) => {
                    const lane = laneFor(e.type);
                    const at = Date.parse(String(e.time ?? ""));
                    const index = ordered.findIndex((o) => o.sequence === e.sequence);
                    const prev = index > 0 ? Date.parse(String(ordered[index - 1].time ?? "")) : NaN;
                    const detail = eventState(e.data) === "" ? eventSubject(e.data) : eventState(e.data);
                    return (
                      // NOT aria-selected. A <tr> is role="row", and aria-selected is only allowed on a row
                      // inside a grid or treegrid — axe's aria-allowed-attr rule (wcag2a) refuses it on a
                      // plain table. The selection is stated where it belongs: aria-pressed on the control
                      // that toggles it.
                      <tr key={e.sequence} data-selected={selected === e.sequence ? "true" : undefined}>
                        <td>
                          <span className="lane-badge" data-lane={lane} data-testid="event-lane">
                            {LANE_LABEL[lane]}
                          </span>
                        </td>
                        <td>
                          <button
                            type="button"
                            className="row-select"
                            aria-pressed={selected === e.sequence}
                            data-testid="event-open"
                            onClick={() => setSelected(selected === e.sequence ? null : e.sequence)}
                          >
                            {e.type}
                          </button>
                          {isFailureEvent(e.type) ? (
                            <span className="pill-error" data-testid="event-error">
                              <span className="glyph" aria-hidden="true">
                                ✖︎
                              </span>{" "}
                              failed
                            </span>
                          ) : null}
                        </td>
                        <td>{detail === "" ? <span className="cell-none">—</span> : <code>{detail}</code>}</td>
                        <td className="num">{Number.isNaN(prev) || Number.isNaN(at) ? "—" : formatDuration(at - prev)}</td>
                        <td className="num">{Number.isNaN(at) || span === null ? "—" : elapsedStamp(at - span.start)}</td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            )}
          </section>

          <aside className="panel detail-pane" aria-labelledby="detail-h" data-testid="event-detail">
            <div className="panel-head">
              <h2 id="detail-h">Event</h2>
              {chosen === null ? null : (
                <div className="panel-tools" role="group" aria-label="Detail view">
                  <button
                    type="button"
                    className="row-select"
                    aria-pressed={view === "rendered"}
                    data-testid="detail-rendered"
                    onClick={() => setView("rendered")}
                  >
                    Rendered
                  </button>
                  <button type="button" className="row-select" aria-pressed={view === "raw"} data-testid="detail-raw" onClick={() => setView("raw")}>
                    Raw
                  </button>
                </div>
              )}
            </div>
            {chosen === null ? (
              <p className="empty" data-testid="detail-empty">
                Choose an event to read what it carries.
              </p>
            ) : view === "raw" ? (
              // RAW IS THE FRAME AS THE JOURNAL SERVED IT, with nothing removed and nothing renamed — which is
              // the only version a reader can check the Rendered one against.
              <pre className="code" data-testid="detail-raw-body">
                {JSON.stringify(chosen, null, 2)}
              </pre>
            ) : (
              <dl data-testid="detail-rendered-body">
                <dt>Type</dt>
                <dd>
                  <code>{chosen.type}</code>
                </dd>
                <dt>Lane</dt>
                <dd>{LANE_LABEL[laneFor(chosen.type)]}</dd>
                <dt>Sequence</dt>
                <dd className="num">{chosen.sequence}</dd>
                <dt>Time</dt>
                <dd>{absoluteTime(String(chosen.time ?? ""))}</dd>
                {/* A FRAGMENT AND NOT A WRAPPER <div>. HTML's own content model for <dl> is groups of dt/dd
                    OR div-wrapped groups, never a mix — and axe's `definition-list` rule carries the wcag2a
                    tag, so a wrapper here would be a violation on the one screen with a generated field
                    list. Fragments keep the dt/dd sequence flat. */}
                {Object.entries(chosen.data ?? {}).map(([key, value]) => (
                  <Fragment key={key}>
                    <dt>{key}</dt>
                    <dd>
                      <code>{typeof value === "string" ? value : JSON.stringify(value)}</code>
                    </dd>
                  </Fragment>
                ))}
              </dl>
            )}
          </aside>
        </div>
      </div>

      <div role="tabpanel" id="panel-debug" aria-labelledby="tab-debug" tabIndex={0} hidden={tab !== "debug"}>
        <section className="panel" aria-labelledby="debug-h" data-testid="session-debug">
          <div className="panel-head">
            <h2 id="debug-h">Debug</h2>
          </div>
          <h3>Session row</h3>
          <pre className="code" data-testid="debug-session">
            {session === null ? "…" : JSON.stringify(session, null, 2)}
          </pre>
          <h3>Journal frames</h3>
          <pre className="code" data-testid="debug-journal">
            {ordered.map((e) => JSON.stringify(e)).join("\n")}
          </pre>
        </section>
      </div>
    </>
  );
}
