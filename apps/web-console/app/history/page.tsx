"use client";

import { useEffect, useState } from "react";

import { Button } from "@/components/ui/Button";
import { Panel } from "@/components/Panel";
import { Status } from "@/components/Status";
import { Timeline, type Frame } from "@/components/Timeline";
import { apiGet, artifactHref, readSessionEvents, RelayError } from "@/lib/api";
import { isTerminal, laneFor } from "@/lib/timeline";

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
export default function HistoryPage() {
  const [selected, setSelected] = useState<RunRow | null>(null);
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [detailError, setDetailError] = useState("");
  const [frames, setFrames] = useState<Frame[]>([]);
  const [replayError, setReplayError] = useState("");

  const sessionID = detail?.session_id ?? selected?.session_id ?? "";

  useEffect(() => {
    setDetail(null);
    setDetailError("");
    if (selected === null) return;
    let live = true;
    apiGet<RunDetail>(`/responses/${encodeURIComponent(selected.id)}`)
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
  }, [selected]);

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

  return (
    <>
      <Panel<RunRow>
        title="Past runs"
        testId="panel-runs"
        fetchPath="/responses"
        note="Newest first, and confined to the tenant this console's key is scoped to. Open a run to read the event journal it wrote and the files it left behind."
        columns={[
          { header: "Response", render: (row) => <code>{row.id}</code> },
          { header: "Status", render: (row) => <Status value={row.status} /> },
          { header: "Created", render: (row) => row.created_at },
          {
            header: "",
            render: (row) => (
              <Button testId="run-open" onClick={() => setSelected(row)}>
                Open
              </Button>
            ),
          },
        ]}
        emptyNote="No run has been started in this project yet. Start one on the Live runs page; it appears here as soon as it is created, and stays after the browser tab that started it is gone — which is the reason this screen exists."
      />

      {selected === null ? (
        <section className="panel" aria-labelledby="history-hint-h">
          <h2 id="history-hint-h">No run selected</h2>
          <p data-testid="history-hint">
            Choose a run above to read its detail, replay the canonical event journal it wrote, and list the
            artifacts it produced.
          </p>
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
              { header: "Created", render: (row) => row.created_at },
            ]}
            emptyNote="This run produced no files. A run only writes an artifact where the deployment gave it a workspace to write into, so an empty list here is the normal outcome for a run that answered in prose."
          />
        </>
      )}
    </>
  );
}
