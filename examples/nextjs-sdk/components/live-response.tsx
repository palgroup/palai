"use client";

import { useReducer, useRef, useState } from "react";

// This component consumes ONLY the Route Handler's newline-delimited canonical projection
// stream (/api/palai). It never imports the SDK's server path and never sees the API key —
// the Route Handler is the sole holder. It renders the ordered event timeline, streamed text,
// tool request/result, usage, the server-selected model, and the final structured output; on
// failure it shows a stable error code + request id. It renders no hidden reasoning.

type Tool = { id?: string; name?: string; arguments?: unknown; result?: unknown };
type Usage = Record<string, number | null>;

type State = {
  status: string;
  text: string;
  timeline: { sequence: number; type: string }[];
  toolRequested: Tool | null;
  toolCompleted: Tool | null;
  usage: Usage | null;
  model: string | null;
  finalOutput: unknown[] | null;
  error: { code: string; requestId: string | null; detail?: string } | null;
};

const initialState: State = {
  status: "idle",
  text: "",
  timeline: [],
  toolRequested: null,
  toolCompleted: null,
  usage: null,
  model: null,
  finalOutput: null,
  error: null,
};

type Projection = Record<string, unknown> & { type: string; sequence?: number };

type Action =
  | { kind: "reset" }
  | { kind: "status"; status: string }
  | { kind: "aborted" }
  | { kind: "clientError"; code: string; detail: string }
  | { kind: "projection"; msg: Projection };

function reducer(state: State, action: Action): State {
  switch (action.kind) {
    case "reset":
      return { ...initialState, status: "streaming" };
    case "status":
      return { ...state, status: action.status };
    case "aborted":
      return { ...state, status: "aborted" };
    case "clientError":
      return { ...state, status: "error", error: { code: action.code, requestId: null, detail: action.detail } };
    case "projection":
      return applyProjection(state, action.msg);
  }
}

function applyProjection(state: State, msg: Projection): State {
  const timeline =
    typeof msg.sequence === "number"
      ? [...state.timeline, { sequence: msg.sequence, type: msg.type }]
      : state.timeline;
  switch (msg.type) {
    case "status":
      return { ...state, status: String(msg.status ?? state.status) };
    case "model_step.delta.v1":
      return { ...state, timeline, text: state.text + String(msg.text ?? "") };
    case "tool_call.proposed.v1":
    case "tool_call.ready.v1":
      return { ...state, timeline, toolRequested: msg.tool as Tool };
    case "tool_call.completed.v1":
      return { ...state, timeline, toolCompleted: msg.tool as Tool };
    case "usage.updated.v1":
      return { ...state, timeline, usage: msg.usage as Usage };
    case "response.final":
      return {
        ...state,
        timeline,
        model: (msg.model as string) ?? state.model,
        finalOutput: (msg.output as unknown[]) ?? null,
        usage: (msg.usage as Usage) ?? state.usage,
        status: String(msg.status ?? "completed"),
        error: msg.error ? (msg.error as State["error"]) : state.error,
      };
    case "error":
      return {
        ...state,
        timeline,
        status: "error",
        error: { code: String(msg.code), requestId: (msg.requestId as string) ?? null, detail: msg.detail as string },
      };
    default:
      return { ...state, timeline };
  }
}

export function LiveResponse() {
  const [prompt, setPrompt] = useState("What is 7 + 5?");
  const [state, dispatch] = useReducer(reducer, initialState);
  const abortRef = useRef<AbortController | null>(null);

  async function run() {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;
    dispatch({ kind: "reset" });

    try {
      const res = await fetch("/api/palai", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ prompt }),
        signal: controller.signal,
      });
      if (!res.ok || res.body === null) {
        const problem = (await res.json().catch(() => ({}))) as { code?: string; detail?: string };
        dispatch({ kind: "clientError", code: problem.code ?? `http_${res.status}`, detail: problem.detail ?? "request failed" });
        return;
      }
      const reader = res.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      for (;;) {
        const { done, value } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let newline: number;
        while ((newline = buffer.indexOf("\n")) !== -1) {
          const line = buffer.slice(0, newline).trim();
          buffer = buffer.slice(newline + 1);
          if (line !== "") dispatch({ kind: "projection", msg: JSON.parse(line) as Projection });
        }
      }
    } catch (err) {
      if (controller.signal.aborted) {
        dispatch({ kind: "aborted" });
      } else {
        dispatch({ kind: "clientError", code: "connection_error", detail: (err as Error).message });
      }
    }
  }

  function abort() {
    abortRef.current?.abort();
  }

  return (
    <main style={styles.main}>
      <h1 style={styles.h1}>Palai SDK — live response proof</h1>
      <p style={styles.note}>
        The browser talks only to this app&apos;s Route Handler, which holds the API key server-side and re-projects
        canonical events. The key never reaches this page.
      </p>

      <div style={styles.row}>
        <input
          data-testid="prompt-input"
          style={styles.input}
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          aria-label="prompt"
        />
        <button data-testid="run-button" style={styles.button} onClick={run}>
          Run
        </button>
        <button data-testid="abort-button" style={styles.button} onClick={abort}>
          Abort
        </button>
      </div>

      <p>
        Status: <strong data-testid="status">{state.status}</strong>
        {state.model !== null && (
          <>
            {" · model: "}
            <strong data-testid="model">{state.model}</strong>
          </>
        )}
      </p>

      <section style={styles.grid}>
        <Panel title="Streamed text">
          <p data-testid="stream-text" style={styles.mono}>
            {state.text}
          </p>
        </Panel>

        <Panel title="Event timeline">
          <ol data-testid="timeline" style={styles.mono}>
            {state.timeline.map((e, i) => (
              <li key={i}>
                {e.sequence}. {e.type}
              </li>
            ))}
          </ol>
        </Panel>

        <Panel title="Tool requested">
          <pre data-testid="tool-requested" style={styles.mono}>
            {state.toolRequested
              ? `${state.toolRequested.name}\n${JSON.stringify(state.toolRequested.arguments, null, 2)}`
              : "—"}
          </pre>
        </Panel>

        <Panel title="Tool completed">
          <pre data-testid="tool-completed" style={styles.mono}>
            {state.toolCompleted ? String(asText(state.toolCompleted.result)) : "—"}
          </pre>
        </Panel>

        <Panel title="Usage">
          <pre data-testid="usage" style={styles.mono}>
            {state.usage ? JSON.stringify(state.usage, null, 2) : "—"}
          </pre>
        </Panel>

        <Panel title="Final output">
          <pre data-testid="final-output" style={styles.mono}>
            {state.finalOutput ? state.finalOutput.map((item) => asText((item as { content?: unknown }).content)).join("\n") : "—"}
          </pre>
        </Panel>
      </section>

      {state.error && (
        <div data-testid="error-panel" style={styles.error}>
          <strong>Error {state.error.code}</strong>
          {state.error.requestId && <span> · request {state.error.requestId}</span>}
          {state.error.detail && <p style={styles.mono}>{state.error.detail}</p>}
        </div>
      )}
    </main>
  );
}

function Panel({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <div style={styles.panel}>
      <h2 style={styles.h2}>{title}</h2>
      {children}
    </div>
  );
}

function asText(value: unknown): string {
  if (value === null || value === undefined) return "—";
  return typeof value === "string" ? value : JSON.stringify(value);
}

const styles: Record<string, React.CSSProperties> = {
  main: { fontFamily: "system-ui, sans-serif", maxWidth: 900, margin: "2rem auto", padding: "0 1rem", color: "#111" },
  h1: { fontSize: "1.4rem" },
  h2: { fontSize: "0.85rem", textTransform: "uppercase", letterSpacing: "0.05em", color: "#555", margin: "0 0 0.5rem" },
  note: { color: "#555", fontSize: "0.9rem" },
  row: { display: "flex", gap: "0.5rem", margin: "1rem 0" },
  input: { flex: 1, padding: "0.5rem", fontSize: "1rem", border: "1px solid #ccc", borderRadius: 6 },
  button: { padding: "0.5rem 1rem", fontSize: "1rem", cursor: "pointer", border: "1px solid #888", borderRadius: 6, background: "#f4f4f4" },
  grid: { display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: "0.75rem" },
  panel: { border: "1px solid #e2e2e2", borderRadius: 8, padding: "0.75rem", background: "#fafafa" },
  mono: { fontFamily: "ui-monospace, monospace", fontSize: "0.85rem", whiteSpace: "pre-wrap", margin: 0 },
  error: { marginTop: "1rem", padding: "0.75rem", border: "1px solid #e0b4b4", borderRadius: 8, background: "#fff6f6", color: "#912d2b" },
};
