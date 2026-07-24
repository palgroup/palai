"use client";

import { useRef, useState } from "react";

import { ApprovalPanel, type PendingApproval } from "@/components/ApprovalPanel";
import { Status } from "@/components/Status";
import { Timeline, type Frame } from "@/components/Timeline";
import { apiGet, artifactHref, RelayError, streamRun } from "@/lib/api";

interface ToolRow {
  seq: number;
  type: string;
  name: string | null;
  arguments: unknown;
  result: unknown;
}
interface RecoveryRow {
  seq: number;
  type: string;
  attempt: unknown;
  detail: unknown;
}
interface Usage {
  input_tokens: number | null;
  output_tokens: number | null;
  total_tokens: number | null;
  tool_calls: number | null;
}
interface ArtifactRow {
  id: string;
}

// The §47.2 live surface: start a run, watch its canonical timeline sorted into lanes, act on an approval
// (the exact-detail panel), see recovery/attempt transitions, read usage, and download artifacts — all
// through the /v1/* relay. The API key never reaches this component; every call is a same-origin relay fetch.
export default function RunsPage() {
  const [prompt, setPrompt] = useState("Push the release branch.");
  const [status, setStatus] = useState<string>("idle");
  const [frames, setFrames] = useState<Frame[]>([]);
  const [streamText, setStreamText] = useState("");
  const [tools, setTools] = useState<ToolRow[]>([]);
  const [recovery, setRecovery] = useState<RecoveryRow[]>([]);
  const [usage, setUsage] = useState<Usage | null>(null);
  const [pending, setPending] = useState<PendingApproval | null>(null);
  const [sessionId, setSessionId] = useState<string>("");
  const [terminal, setTerminal] = useState<{ status: string; model: string | null; output: unknown } | null>(null);
  const [artifacts, setArtifacts] = useState<ArtifactRow[]>([]);
  const [running, setRunning] = useState(false);

  const responseIdRef = useRef<string>("");
  const abortRef = useRef<AbortController | null>(null);

  function reset() {
    setStatus("streaming");
    setFrames([]);
    setStreamText("");
    setTools([]);
    setRecovery([]);
    setUsage(null);
    setPending(null);
    setSessionId("");
    setTerminal(null);
    setArtifacts([]);
    responseIdRef.current = "";
  }

  function onFrame(f: Record<string, unknown>) {
    const type = f.type as string | undefined;
    if (type === "status") {
      setStatus(String(f.status));
      return;
    }
    if (type === "meta") {
      setSessionId(String(f.sessionId ?? ""));
      responseIdRef.current = String(f.responseId ?? "");
      return;
    }
    if (type === "final") {
      setStatus(String(f.status));
      setTerminal({ status: String(f.status), model: (f.model as string) ?? null, output: f.output });
      if (f.usage) setUsage(f.usage as Usage);
      return;
    }
    if (type === "error") {
      setStatus("error");
      setTerminal({ status: "error", model: null, output: f.detail });
      return;
    }

    // A lane-tagged canonical event: record it for the timeline, then fan out to its lane view.
    setFrames((prev) => [...prev, f as Frame]);
    const lane = f.lane as string | undefined;
    if (lane === "model_step" && typeof f.text === "string") {
      setStreamText((prev) => prev + f.text);
    } else if (lane === "tool") {
      const t = f.tool as Record<string, unknown>;
      setTools((prev) => [...prev, { seq: Number(f.sequence), type: String(type), name: (t?.name as string) ?? null, arguments: t?.arguments, result: t?.result }]);
    } else if (lane === "approval") {
      const a = f.approval as PendingApproval & { resolution: string | null };
      if (a.resolution === null) {
        setPending(a);
      } else {
        setPending(null); // resolved — the authoritative detail is no longer actionable
      }
    } else if (lane === "recovery") {
      const rec = f.recovery as Record<string, unknown>;
      setRecovery((prev) => [...prev, { seq: Number(f.sequence), type: String(type), attempt: rec?.attempt, detail: rec?.detail }]);
    } else if (lane === "usage") {
      setUsage(f.usage as Usage);
    }
  }

  async function run() {
    reset();
    setRunning(true);
    const controller = new AbortController();
    abortRef.current = controller;
    try {
      await streamRun(prompt, onFrame, controller.signal);
      // The run reached its terminal — load the artifacts it produced (through the relay).
      const rid = responseIdRef.current;
      if (rid !== "") {
        try {
          const list = await apiGet<{ data?: ArtifactRow[] }>(`/responses/${encodeURIComponent(rid)}/artifacts`);
          setArtifacts(list.data ?? []);
        } catch {
          /* an empty/absent artifact list is not fatal to the timeline */
        }
      }
    } catch (err) {
      if (!controller.signal.aborted) {
        setStatus("error");
        setTerminal({ status: "error", model: null, output: err instanceof RelayError ? err.problem.detail : "stream failed" });
      }
    } finally {
      setRunning(false);
    }
  }

  function abort() {
    abortRef.current?.abort();
    setStatus("aborted");
    setRunning(false);
  }

  return (
    <>
      <section className="panel" aria-labelledby="run-h">
        <h2 id="run-h">Start a run</h2>
        <label htmlFor="prompt-input">Prompt</label>
        <textarea id="prompt-input" data-testid="prompt-input" rows={2} value={prompt} onChange={(e) => setPrompt(e.target.value)} />
        <p>
          <button type="button" data-testid="run-button" onClick={run} disabled={running}>
            Run
          </button>{" "}
          <button type="button" data-testid="abort-button" onClick={abort} disabled={!running}>
            Abort
          </button>{" "}
          <Status value={status} testId="status" />
        </p>
      </section>

      {pending && sessionId ? <ApprovalPanel approval={pending} sessionId={sessionId} onResolved={() => setPending(null)} /> : null}

      <section className="panel" aria-labelledby="message-h">
        <h2 id="message-h">Assistant message</h2>
        <p data-testid="stream-text">{streamText}</p>
      </section>

      <section className="panel" aria-labelledby="tools-h">
        <h2 id="tools-h">Tool &amp; subagent activity</h2>
        {tools.length === 0 ? (
          <p className="muted">None yet.</p>
        ) : (
          <ul data-testid="tool-activity">
            {tools.map((t, i) => (
              <li key={i}>
                <strong>{t.type}</strong> {t.name ?? ""}
                {t.arguments ? <pre className="code">{JSON.stringify(t.arguments)}</pre> : null}
                {t.result !== null && t.result !== undefined ? <span data-testid="tool-result"> → {JSON.stringify(t.result)}</span> : null}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="recovery-h">
        <h2 id="recovery-h">Recovery &amp; attempts</h2>
        {recovery.length === 0 ? (
          <p className="muted">No recovery transitions.</p>
        ) : (
          <ul data-testid="recovery-display">
            {recovery.map((r, i) => (
              <li key={i}>
                <Status value={r.type} /> {r.detail ? String(r.detail) : ""}
              </li>
            ))}
          </ul>
        )}
      </section>

      <section className="panel" aria-labelledby="usage-h">
        <h2 id="usage-h">Usage</h2>
        <p data-testid="usage">{usage ? `total ${usage.total_tokens ?? 0} tokens, ${usage.tool_calls ?? 0} tool calls` : "—"}</p>
      </section>

      {terminal ? (
        <section className="panel" aria-labelledby="terminal-h">
          <h2 id="terminal-h">Terminal result</h2>
          <p>
            <Status value={terminal.status} testId="terminal-status" />
          </p>
          <p data-testid="model">Model: {terminal.model ?? "—"}</p>
          <pre className="code" data-testid="final-output">
            {JSON.stringify(terminal.output, null, 2)}
          </pre>
        </section>
      ) : null}

      <section className="panel" aria-labelledby="artifacts-h">
        <h2 id="artifacts-h">Artifacts</h2>
        {artifacts.length === 0 ? (
          <p className="muted">No artifacts.</p>
        ) : (
          <ul data-testid="artifact-list">
            {artifacts.map((a) => (
              <li key={a.id}>
                <a data-testid="artifact-download" href={artifactHref(a.id)} download>
                  {a.id}
                </a>
              </li>
            ))}
          </ul>
        )}
      </section>

      <Timeline frames={frames} />
    </>
  );
}
