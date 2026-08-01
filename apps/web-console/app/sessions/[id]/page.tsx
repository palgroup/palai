"use client";

import { usePathname, useParams, useRouter, useSearchParams } from "next/navigation";
import { Fragment, useEffect, useMemo, useState } from "react";

import { Button } from "@/components/ui/Button";
import { LaneStrip } from "@/components/LaneStrip";
import { AgentChips, CopyButton, RenameSession, SessionName, shortId, Stamp } from "@/components/Session";
import { Select } from "@/components/ui/Select";
import { TabPanel, Tabs } from "@/components/ui/Tabs";
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
  spanOf,
  type JournalEvent,
  type SessionRow,
} from "@/lib/sessions";
import { staveOf } from "@/lib/strip";
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

  // THE SELECTED EVENT AND THE OPEN TAB LIVE IN THE URL, NOT IN REACT STATE — the reference console's
  // `…/sessions/sesn_01NRK…?event=sevt_01XUF…&segment=debug`, and the single largest reason a console feels
  // dead without it. Three things follow that no amount of local state gives: the back button undoes a
  // selection, a row can be sent to somebody as a link, and a reload lands where the reader was. NOTHING
  // else in this console puts screen state in the URL; these two parameters are the first.
  //
  // The URL is the SOURCE, not a mirror: `selected` and `tab` are read out of it on every render rather than
  // being state that is also written to the address bar, so the two can never disagree.
  const router = useRouter();
  const pathname = usePathname();
  const search = useSearchParams();
  const selected = Number(search.get("event") ?? "") || null;
  const tab: Tab = search.get("segment") === "debug" ? "debug" : "transcript";

  const [session, setSession] = useState<SessionRow | null>(null);
  const [sessionError, setSessionError] = useState("");
  const [events, setEvents] = useState<JournalEvent[]>([]);
  const [journalError, setJournalError] = useState("");
  const [typeFilter, setTypeFilter] = useState("");
  const [query, setQuery] = useState("");
  const [view, setView] = useState<"rendered" | "raw">("rendered");
  const [renaming, setRenaming] = useState(false);

  /**
   * setParams writes the screen's state to the address bar.
   *
   * `push` rather than `replace`, deliberately: the measured behaviour is that the BACK BUTTON undoes a
   * selection, and a replace would make the browser's own affordance leave the page instead. A parameter set
   * to null is DELETED rather than emptied — `?event=` with nothing after it is a URL that says a row is
   * selected and does not say which.
   */
  function setParams(next: Record<string, string | null>) {
    const params = new URLSearchParams(search.toString());
    for (const [key, value] of Object.entries(next)) {
      if (value === null) params.delete(key);
      else params.set(key, value);
    }
    const query = params.toString();
    // scroll:false — selecting an event must not jump the reader to the top of a fourteen-row transcript.
    router.push(query === "" ? pathname : `${pathname}?${query}`, { scroll: false });
  }

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

  function chooseTab(next: Tab) {
    setParams({ segment: next === "transcript" ? null : next });
  }

  return (
    <>
      <nav aria-label="Breadcrumb" className="breadcrumb">
        <ol>
          <li>
            <a href="/sessions">Sessions</a>
          </li>
          {/* THE BREADCRUMB'S ID IS A COPY BUTTON, NOT TEXT — measured. It is the one place an operator
              reliably has the session id in front of them, and selecting 36 characters out of a breadcrumb
              by hand is the interaction it replaces. */}
          <li aria-current="page">
            <CopyButton value={sessionID} label="session ID" testId="breadcrumb-session">
              <code>{shortId(sessionID)}</code>
            </CopyButton>
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

      {/* THE TITLE ITSELF IS THE RENAME AFFORDANCE (§3, measured: "başlığın KENDİSİ bir düğme"), which is why
          there is no separate Rename control beside it. The accessible name APPENDS the action to the
          visible one rather than replacing it: SC 2.5.3 Label in Name requires the accessible name to
          contain the label, so an aria-label of "Rename session" over a button reading "Release rehearsal 1"
          would fail — the sr-only suffix is what makes this both discoverable and conforming. */}
      <div className="session-head">
        <h1 className="page-title" data-testid="session-title">
          {session === null ? (
            <code>{shortId(sessionID)}</code>
          ) : renaming ? (
            <SessionName name={session.name} source={session.name_source} />
          ) : (
            <Button className="title-rename" testId="session-rename-open" onClick={() => setRenaming(true)}>
              <SessionName name={session.name} source={session.name_source} />
              <span className="glyph" aria-hidden="true">
                ✎
              </span>
              <span className="sr-only"> — rename this session</span>
            </Button>
          )}
        </h1>
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
      {/* THE CHIP ROW CARRIES THIS ROUTE'S READINESS SIGNAL (lib/routes.ts), so it must render in BOTH settled
          states and in neither pending one. A failed session read used to leave "Loading…" on screen forever
          — the same conflation app/agents/[id]/page.tsx refuses between a spinner and a settled panel — and
          with the signal pointed here that would be a scan that never fires rather than one that fires early.
          The refusal itself is announced above, in the server's own words; this only stops claiming to load. */}
      {sessionError !== "" ? (
        <ul className="chip-row" data-testid="session-chips">
          <li>
            <span className="chip" data-testid="chip-status">
              <span className="chip-key">Session</span> <span className="cell-none">— unreadable</span>
            </span>
          </li>
        </ul>
      ) : session === null ? (
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

      <Tabs label="Session views" tabs={TABS} value={tab} onValueChange={chooseTab}>
        <TabPanel value="transcript">
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
                  <Select
                    label="Event type"
                    testId="transcript-type"
                    value={typeFilter}
                    onValueChange={setTypeFilter}
                    options={[{ value: "", label: "All event types" }, ...types.map((t) => ({ value: t, label: t }))]}
                  />
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

              {/* THE SCRUBBER IS NOW THE STAVE, AND IT IS THE SAME OBJECT IT ALWAYS WAS — extended, not
                  replaced. It has always placed one mark per frame by that frame's own timestamp, so a burst
                  of five in one second and a four-minute wait for a model read like the two different things
                  they are. What changes is that a mark no longer sits in one undifferentiated track: every
                  §47.2 LANE gets a channel of its own, named in the gutter with its count, and a mark's
                  vertical position is what says which lane it belongs to. The colour is the third carrier
                  after position and word, which is the rule every other state signal in this console follows.

                  IT IS STILL aria-hidden AND ITS CAPTION IS STILL NOT. A track of absolutely-positioned marks
                  carries no text; announcing it would read out fourteen empty elements. Everything it shows
                  is in the caption, in the Lane column and in the Elapsed column of every row below — the
                  strip is a way of seeing the table faster, never a place information only lives. */}
              {span === null ? null : (
                <LaneStrip
                  scale="stave"
                  testId="transcript-scrubber"
                  captionTestId="scrubber-caption"
                  channels={staveOf(
                    ordered.map((e) => ({
                      key: String(e.sequence),
                      lane: laneFor(e.type),
                      time: String(e.time ?? ""),
                      failure: isFailureEvent(e.type),
                    })),
                    span,
                  )}
                  axis={["0:00:00", elapsedStamp(span.end - span.start)]}
                  caption={
                    <>
                      {ordered.length} {ordered.length === 1 ? "event" : "events"} over {formatDuration(span.end - span.start)}, ending at{" "}
                      {elapsedStamp(span.end - span.start)}
                    </>
                  }
                />
              )}

              {ordered.length === 0 ? (
                <div className="empty" data-testid="transcript-empty">
                  <p className="empty-title">No events yet</p>
                  <p className="empty-body">
                    A session&apos;s journal records every state its runs passed through, from queued to
                    completed — this one has not run.
                  </p>
                </div>
              ) : shown.length === 0 ? (
                <div className="empty" data-testid="transcript-no-match">
                  <p className="empty-title">No matching events</p>
                  <p className="empty-body">This session journalled {ordered.length} events; none of them matches the filters above.</p>
                </div>
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
                            <Button
                              className="row-select"
                              aria-pressed={selected === e.sequence}
                              testId="event-open"
                              onClick={() => setParams({ event: selected === e.sequence ? null : String(e.sequence) })}
                            >
                              {e.type}
                            </Button>
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
              <div className="detail-head">
                <h2 id="detail-h">Event</h2>
                {/* THE CLOSE CONTROL, AND IT DROPS ?event= FROM THE URL (§3 decision 5). A pane whose only way
                    out is clicking the same row again is a pane an operator closes by reloading. */}
                {chosen === null ? null : (
                  <Button className="detail-close" testId="detail-close" onClick={() => setParams({ event: null })}>
                    Close
                    <span className="sr-only"> detail panel</span>
                  </Button>
                )}
              </div>
              {/* DISABLED, NOT HIDDEN (§3 decision 4) — and this is the reference's habit replacing ours. This
                  pair used to render only once an event was chosen, so the affordance did not exist until
                  after the interaction that needs it, and an operator had no way to know the raw frame was
                  available at all. `Deltas` is the reference's third option and is ABSENT rather than
                  disabled: our journal has no per-event delta view — model_step.delta.v1 is its own FRAME, not
                  a rendering of another one — so a third control would be a control that could never enable,
                  which is a different lie from the one this rule fixes. */}
              <div className="panel-tools" role="group" aria-label="Detail view">
                <Button
                  className="row-select"
                  aria-pressed={view === "rendered"}
                  disabled={chosen === null}
                  testId="detail-rendered"
                  onClick={() => setView("rendered")}
                >
                  Rendered
                </Button>
                <Button
                  className="row-select"
                  aria-pressed={view === "raw"}
                  disabled={chosen === null}
                  testId="detail-raw"
                  onClick={() => setView("raw")}
                >
                  Raw
                </Button>
              </div>
              {chosen === null ? (
                <div className="empty" data-testid="detail-empty">
                  <p className="empty-title">No event selected</p>
                  <p className="empty-body">
                    Choose a row to read the frame the journal recorded — its fields laid out, or the raw JSON
                    the API served.
                  </p>
                </div>
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
        </TabPanel>

        <TabPanel value="debug">
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
        </TabPanel>
      </Tabs>
    </>
  );
}
