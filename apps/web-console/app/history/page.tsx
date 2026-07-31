"use client";

import { useEffect, useState } from "react";

import { Panel, type Column } from "@/components/Panel";
import { CopyButton, shortId, Stamp } from "@/components/Session";
import { Status } from "@/components/Status";
import { Timeline, type Frame } from "@/components/Timeline";
import { apiGet, artifactHref, readSessionEvents, RelayError } from "@/lib/api";
import { isTerminal, laneFor } from "@/lib/timeline";
import { useQueryParam } from "@/lib/urlState";

interface RunRow extends Record<string, unknown> {
  id: string;
  status: string;
  session_id?: string;
  created_at: string;
}

interface RunDetail {
  id: string;
  status: string;
  model: string;
  session_id?: string;
  created_at: string;
  updated_at?: string;
  output: unknown;
  usage?: { input_tokens?: number; output_tokens?: number; total_tokens?: number; tool_calls?: number };
}

interface ArtifactRow extends Record<string, unknown> {
  id: string;
  media_type: string;
  size_bytes: number;
  checksum: string;
  logical_type: string;
  malware_scan_status: string;
  created_at: string;
}

// O1 + O2 + O6 — RUN HISTORY (feature list §7, plan §T8). Until now the console could START a run and watch
// it stream, and the moment the browser tab closed the run was unreachable from every screen: /runs is a
// prompt box and a live timeline, and nothing listed a past run even though GET /v1/responses has been
// mounted since E13.
//
// Three routes, one page, because all three are keyed by the SAME run and a screen that made an operator
// carry a response id between three pages would be a worse console than one page with a selection:
//
//   O1  GET /v1/responses            the list, newest first, twenty to a page with the cut stated in words
//       GET /v1/responses/{id}       the detail — the list row carries only the durable columns
//   O2  GET /v1/sessions/{id}/events the journal, replayed; the ONLY way to read a finished run's events
//   O6  GET /v1/responses/{id}/artifacts   what the run left behind, downloadable through the HARDENED relay
//
// THE DOWNLOAD LINK IS THE RELAY'S, AND THAT IS THE WHOLE OF THIS PAGE'S SECURITY POSTURE. Artifact bytes
// are untrusted and the console's origin holds the operator session, so the bytes are served only through
// /api/palai/v1/artifacts/{id}/content, which coerces the type, sets nosniff, forces an attachment
// disposition, sanitizes the filename and denies every content source. E25 T8 changed none of that.
//
// WHAT THE PAGE-PARITY PASS CHANGED, measured on the built console (2026-07-31): four columns, of which the
// first was a raw 36-character response id in mono and the third was a raw ISO timestamp, plus an "Open"
// button in a column of its own — and a selection that lived in React state, so the back button did nothing,
// a run could not be sent to a colleague as a link, and a reload lost the run being read.
//
// THE SELECTION IS IN THE URL NOW (`?run=…`), which is the pattern app/sessions/[id]/page.tsx landed and the
// third screen in this console to use it. Everything below the table is unchanged: this page's detail,
// journal replay and artifact list were already the right shape, and the pass touched the LIST.
//
// THE SESSION CELL IS A LINK, and it is the cross-reference this console did not have: a run belongs to a
// session, /sessions/{id} replays that session's whole journal, and until now an operator holding a response
// id had no way to reach the conversation it came from without retyping an id.

export default function HistoryPage() {
  // lib/urlState.ts rather than next/navigation's useSearchParams, for the reason its own header gives and
  // that app/approvals/page.tsx records having paid for: this route is STATICALLY PRERENDERED, and the
  // Suspense boundary useSearchParams needs there puts its FALLBACK in the served HTML — so the page stops
  // server-rendering its markup. `push`, because every write happens because somebody opened a run.
  const [selectedID, choose] = useQueryParam("run", "push");
  const [rows, setRows] = useState<RunRow[]>([]);
  // The selected ROW comes out of the list this page just read; there is no per-run read that would answer
  // one before the list lands, and the detail fetch below is keyed by the id rather than by the row.
  const selected = rows.find((r) => r.id === selectedID) ?? null;
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [detailError, setDetailError] = useState("");
  const [frames, setFrames] = useState<Frame[]>([]);
  const [replayError, setReplayError] = useState("");

  const sessionID = detail?.session_id ?? selected?.session_id ?? "";

  // KEYED ON THE ID, NOT THE ROW. `selected` is derived from the list on every render, so a new object
  // identity arrives with every poll and an effect depending on it would refetch the detail forever.
  useEffect(() => {
    setDetail(null);
    setDetailError("");
    if (selectedID === "") return;
    let live = true;
    apiGet<RunDetail>(`/responses/${encodeURIComponent(selectedID)}`)
      .then((body) => {
        if (live) setDetail(body);
      })
      .catch((err: unknown) => {
        // The server's own sentence. A 410 for a reaped run and a 404 for an unknown one mean different
        // things — one says the content was retained and then aged out, the other says it was never here —
        // and "failed to load" says neither.
        if (live) setDetailError(err instanceof RelayError ? err.problem.detail : "the run's detail could not be read");
      });
    return () => {
      live = false;
    };
  }, [selectedID]);

  useEffect(() => {
    setFrames([]);
    setReplayError("");
    if (sessionID === "") return;
    const controller = new AbortController();
    readSessionEvents(
      sessionID,
      (event) => {
        const type = String(event.type ?? "");
        if (type === "") return;
        setFrames((prev) => [...prev, { ...event, type, lane: laneFor(type), sequence: Number(event.sequence ?? 0) }]);
        // A FINISHED run's stream closes on its own (api/events.go's pump returns after a terminal event).
        // This aborts anyway, for the run that is still going: opening a queued or running row from the
        // history list would otherwise tail it with heartbeats for as long as the page stayed open, and this
        // screen is history rather than a second live view.
        if (isTerminal(type)) controller.abort();
      },
      controller.signal,
    ).catch((err: unknown) => {
      if (controller.signal.aborted) return;
      setReplayError(err instanceof RelayError ? err.problem.detail : "the run's event journal could not be read");
    });
    return () => {
      controller.abort();
    };
  }, [sessionID]);

  const columns: Column<RunRow>[] = [
    {
      header: "ID",
      sort: (row) => row.id,
      // A SHORT FORM PLUS A COPY BUTTON. It was a raw 36-character id in mono, which an operator can neither
      // read nor reliably select — and which was not the control that opened the run either, so the one cell
      // carrying the row's identity was the one cell that did nothing.
      render: (row) => (
        <span className="cell-id-group">
          <button
            type="button"
            className="row-select cell-id"
            aria-pressed={selectedID === row.id}
            data-testid="run-open"
            data-run-id={row.id}
            title={row.id}
            onClick={() => choose(selectedID === row.id ? "" : row.id)}
          >
            {shortId(row.id)}
          </button>
          <CopyButton value={row.id} label="response ID" testId="run-copy-id" />
        </span>
      ),
    },
    { header: "Status", sort: (row) => row.status, render: (row) => <Status value={row.status} testId="run-status" /> },
    {
      // THE CROSS-REFERENCE THIS CONSOLE DID NOT HAVE. A run belongs to a session and /sessions/{id} replays
      // that session's whole journal; an operator holding a response id had no way to reach the conversation
      // it came from without retyping an id by hand.
      header: "Session",
      render: (row) =>
        row.session_id === undefined || row.session_id === "" ? (
          <span className="cell-none">— none</span>
        ) : (
          <a className="cell-id" href={`/sessions/${encodeURIComponent(row.session_id)}`} title={row.session_id} data-testid="run-session-link">
            {shortId(row.session_id)}
          </a>
        ),
    },
    // RELATIVE, WITH THE ABSOLUTE STAMP IN THE TITLE AND THE datetime — it was the raw ISO string, which is
    // the one rendering a reader has to do arithmetic on.
    { header: "Created", sort: (row) => row.created_at, render: (row) => <Stamp iso={row.created_at} /> },
  ];

  return (
    <>
      <Panel<RunRow>
        title="Past runs"
        testId="panel-runs"
        fetchPath="/responses"
        onRows={setRows}
        columns={columns}
        matchOn={(row) => `${row.id} ${String(row.session_id ?? "")}`}
        filterLabel="Search runs by response or session ID"
        filterPlaceholder="Response or session ID…"
        emptyNote={
          <>
            <p className="empty-title" data-testid="history-empty-title">
              No runs yet
            </p>
            <p className="empty-body">
              A run is one request this project made of a model — what it was asked, the events it journalled
              and the files it left behind. It stays here after the browser tab that started it is gone, which
              is the reason this screen exists.
            </p>
            <p className="empty-body">
              <a href="/runs">Start one on Live runs</a>.
            </p>
          </>
        }
      />

      {selected === null ? (
        <section className="panel" aria-labelledby="history-hint-h">
          {/* The region's name, not the state's. The empty state below carries the state, and two headings
              saying "No run selected" one above the other is the same duplication app/approvals/page.tsx
              removed an hour earlier. */}
          <h2 id="history-hint-h">Run detail</h2>
          <div className="empty" data-testid="history-hint">
            <p className="empty-title">{selectedID === "" ? "No run selected" : "That run is not on this page"}</p>
            <p className="empty-body">
              {selectedID === ""
                ? "Choose a run above to read its detail, replay the canonical event journal it wrote, and list the artifacts it produced."
                : "The list is cut at twenty rows and this run is past the cut, or it belongs to another project. Load more above, or search for its id."}
            </p>
          </div>
        </section>
      ) : (
        <>
          <section className="panel" aria-labelledby="run-detail-h">
            <h2 id="run-detail-h">Run detail</h2>
            {detailError === "" ? null : (
              <p role="alert" className="form-error" data-testid="run-detail-error">
                <span className="glyph" aria-hidden="true">
                  ✖
                </span>{" "}
                {detailError}
              </p>
            )}
            <dl data-testid="run-detail">
              <dt>Response</dt>
              <dd>{selected.id}</dd>
              <dt>Status</dt>
              <dd>
                <Status value={detail?.status ?? selected.status} testId="run-detail-status" />
              </dd>
              <dt>Model</dt>
              <dd data-testid="run-detail-model">{detail === null ? "…" : detail.model === "" ? "—" : detail.model}</dd>
              <dt>Session</dt>
              <dd>{sessionID === "" ? "—" : sessionID}</dd>
              <dt>Created</dt>
              <dd>
                <Stamp iso={selected.created_at} />
              </dd>
              <dt>Usage</dt>
              <dd data-testid="run-detail-usage">
                {detail?.usage === undefined
                  ? "—"
                  : `${String(detail.usage.total_tokens ?? 0)} tokens, ${String(detail.usage.tool_calls ?? 0)} tool calls`}
              </dd>
            </dl>
            {detail === null ? null : (
              <pre className="code" data-testid="run-detail-output">
                {JSON.stringify(detail.output, null, 2)}
              </pre>
            )}
          </section>

          {replayError === "" ? null : (
            <p role="alert" className="form-error" data-testid="run-replay-error">
              <span className="glyph" aria-hidden="true">
                ✖
              </span>{" "}
              {replayError}
            </p>
          )}
          {/* The journal, re-read rather than remembered. Nothing on this page saw the run happen. */}
          <Timeline frames={frames} title="Replayed run timeline" testId="past-run-timeline" />

          <Panel<ArtifactRow>
            title="Artifacts"
            testId="panel-run-artifacts"
            fetchPath={`/responses/${encodeURIComponent(selected.id)}/artifacts`}
            note={
              <>
                Downloads go through this console&apos;s relay, which serves every artifact as an{" "}
                <strong>attachment</strong> with a coerced content type and <code>nosniff</code>. Artifact
                bytes are a run&apos;s own output and are never rendered on this origin.
              </>
            }
            columns={[
              {
                header: "Artifact",
                // THERE IS NO FILENAME TO SHOW, and that is measured rather than an omission: the artifact
                // projection (artifacts/reader.go metadataRow.projection) carries id, run_id, size_bytes,
                // checksum, media_type, logical_type, malware_scan_status and created_at — and no name. The
                // download's filename comes from the object store's own disposition, sanitized by the relay.
                render: (row) => (
                  <a data-testid="artifact-download" href={artifactHref(row.id)} download>
                    {row.id}
                  </a>
                ),
              },
              { header: "Type", render: (row) => (row.media_type === "" ? "—" : row.media_type) },
              { header: "Size", render: (row) => <span className="num">{`${String(row.size_bytes)} bytes`}</span> },
              { header: "Scan", render: (row) => row.malware_scan_status },
              { header: "Created", render: (row) => <Stamp iso={row.created_at} /> },
            ]}
            emptyNote="This run produced no files. A run only writes an artifact where the deployment gave it a workspace to write into, so an empty list here is the normal outcome for a run that answered in prose."
          />
        </>
      )}
    </>
  );
}
