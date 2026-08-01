"use client";

import { useState } from "react";

// This component talks ONLY to /api/palai/compare. Like live-response.tsx it never imports the
// SDK's server path and never sees the API key — both halves of the comparison run server-side,
// and that is itself part of what the panel says on screen.

type Outcome = {
  ok: boolean;
  transport: string;
  responseId: string | null;
  status: string | null;
  model: string | null;
  text: string | null;
  usage: Record<string, unknown> | null;
  elapsedMs: number;
  steps: string[];
  handledForYou: string[];
  error: { code: string; requestId: string | null; detail: string } | null;
};

type Result = { mode: string; sessionId: string | null; sdk: Outcome; raw: Outcome };

export function ComparePanel() {
  const [prompt, setPrompt] = useState("In one short sentence, what is a control plane?");
  const [mode, setMode] = useState<"single" | "session">("single");
  const [omitKey, setOmitKey] = useState(false);
  const [busy, setBusy] = useState(false);
  const [result, setResult] = useState<Result | null>(null);
  const [clientError, setClientError] = useState<string | null>(null);

  async function run() {
    setBusy(true);
    setResult(null);
    setClientError(null);
    try {
      const res = await fetch("/api/palai/compare", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ prompt, mode, omitIdempotencyKey: omitKey }),
      });
      const body = await res.json();
      if (!res.ok) {
        setClientError(body?.detail ?? `HTTP ${res.status}`);
        return;
      }
      setResult(body as Result);
    } catch (err) {
      setClientError(err instanceof Error ? err.message : "request failed");
    } finally {
      setBusy(false);
    }
  }

  return (
    <main style={{ fontFamily: "ui-sans-serif, system-ui, sans-serif", maxWidth: 1100, margin: "2rem auto", padding: "0 1rem" }}>
      <h1 style={{ marginBottom: "0.25rem" }}>Same call, two ways</h1>
      <p style={{ color: "#666", marginTop: 0 }}>
        The identical operation through <code>@palai/sdk</code> and through a bare <code>fetch</code> to{" "}
        <code>/v1/responses</code>, so the difference is visible rather than asserted.
      </p>

      {/* THE CLARIFICATION THAT KEEPS THE DEMO HONEST. Without it the panel implies a browser may
          call the control plane directly, which is precisely the thing Palai dropped. */}
      <p data-testid="both-server-side" style={{ background: "#fff8e1", border: "1px solid #ffe0a3", padding: "0.6rem 0.8rem", borderRadius: 6 }}>
        <strong>Both halves run on the server.</strong> This is raw HTTP vs the SDK — not browser vs server. The API
        key never reaches this page; the browser talks only to <code>/api/palai/compare</code>. Palai has no
        browser-direct token by design.
      </p>

      <div style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap", margin: "1rem 0" }}>
        <input
          data-testid="compare-prompt"
          value={prompt}
          onChange={(e) => setPrompt(e.target.value)}
          style={{ flex: "1 1 340px", padding: "0.5rem", fontSize: "1rem" }}
          aria-label="Prompt"
        />
        <label style={{ display: "flex", gap: "0.35rem", alignItems: "center" }}>
          <input
            data-testid="mode-single"
            type="radio"
            name="mode"
            checked={mode === "single"}
            onChange={() => setMode("single")}
          />
          single-shot
        </label>
        <label style={{ display: "flex", gap: "0.35rem", alignItems: "center" }}>
          <input
            data-testid="mode-session"
            type="radio"
            name="mode"
            checked={mode === "session"}
            onChange={() => setMode("session")}
          />
          in a session
        </label>
        <button data-testid="compare-run" onClick={run} disabled={busy} style={{ padding: "0.5rem 1rem", fontSize: "1rem" }}>
          {busy ? "Running…" : "Run both"}
        </button>
      </div>

      <label style={{ display: "flex", gap: "0.35rem", alignItems: "center", color: "#666", marginBottom: "1rem" }}>
        <input data-testid="omit-key" type="checkbox" checked={omitKey} onChange={(e) => setOmitKey(e.target.checked)} />
        Omit <code>Idempotency-Key</code> on the raw path (the SDK always sends one — this shows the 400)
      </label>

      <p style={{ color: "#666" }}>
        <strong>single-shot</strong> creates no session: each half is a standalone run.{" "}
        <strong>in a session</strong> opens one session and posts both turns into it, so they share a conversation.
      </p>

      {clientError ? (
        <p data-testid="compare-client-error" style={{ color: "#b00" }}>
          {clientError}
        </p>
      ) : null}

      {result ? (
        <>
          {result.sessionId ? (
            <p data-testid="compare-session-id" style={{ color: "#666" }}>
              session: <code>{result.sessionId}</code>
            </p>
          ) : (
            <p data-testid="compare-no-session" style={{ color: "#666" }}>
              no session — single-shot
            </p>
          )}
          <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(320px, 1fr))", gap: "1rem" }}>
            <OutcomeCard testid="outcome-sdk" title="via @palai/sdk" outcome={result.sdk} />
            <OutcomeCard testid="outcome-raw" title="via raw fetch" outcome={result.raw} />
          </div>
        </>
      ) : null}
    </main>
  );
}

function OutcomeCard({ testid, title, outcome }: { testid: string; title: string; outcome: Outcome }) {
  return (
    <section data-testid={testid} style={{ border: "1px solid #ddd", borderRadius: 8, padding: "1rem" }}>
      <h2 style={{ marginTop: 0, fontSize: "1.1rem" }}>{title}</h2>
      <dl style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "0.25rem 0.75rem", margin: 0 }}>
        <dt style={{ color: "#666" }}>status</dt>
        <dd data-testid={`${testid}-status`} style={{ margin: 0 }}>
          {outcome.status ?? "—"}
        </dd>
        <dt style={{ color: "#666" }}>model</dt>
        <dd style={{ margin: 0 }}>{outcome.model ?? "—"}</dd>
        <dt style={{ color: "#666" }}>elapsed</dt>
        <dd style={{ margin: 0 }}>{outcome.elapsedMs} ms</dd>
        <dt style={{ color: "#666" }}>response id</dt>
        <dd style={{ margin: 0, wordBreak: "break-all" }}>{outcome.responseId ?? "—"}</dd>
      </dl>

      {outcome.error ? (
        <p data-testid={`${testid}-error`} style={{ color: "#b00", marginTop: "0.75rem" }}>
          <strong>{outcome.error.code}</strong> — {outcome.error.detail}
          {outcome.error.requestId ? ` (request ${outcome.error.requestId})` : ""}
        </p>
      ) : (
        <p data-testid={`${testid}-text`} style={{ marginTop: "0.75rem", whiteSpace: "pre-wrap" }}>
          {outcome.text ?? "—"}
        </p>
      )}

      {outcome.usage ? (
        <p style={{ color: "#666", fontSize: "0.9rem" }}>
          usage: {JSON.stringify(outcome.usage)}
        </p>
      ) : null}

      <details style={{ marginTop: "0.5rem" }}>
        <summary style={{ cursor: "pointer" }}>what this path did</summary>
        <ul style={{ fontSize: "0.9rem", color: "#444" }}>
          {outcome.steps.map((s) => (
            <li key={s}>
              <code>{s}</code>
            </li>
          ))}
        </ul>
        <ul style={{ fontSize: "0.9rem", color: "#444" }}>
          {outcome.handledForYou.map((s) => (
            <li key={s}>{s}</li>
          ))}
        </ul>
      </details>
    </section>
  );
}
