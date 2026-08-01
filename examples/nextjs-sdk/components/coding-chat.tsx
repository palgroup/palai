"use client";

import { useChat } from "@ai-sdk/react";
import { DefaultChatTransport } from "ai";
import { useRef, useState } from "react";

// THE ECOSYSTEM'S CHAT UI, ON PALAI. This component uses @ai-sdk/react's useChat unmodified — the same hook
// a Vercel AI SDK app uses against an OpenAI route — and everything Palai-specific lives behind
// /api/chat, which translates Palai's journal frames into UI-message-stream parts.
//
// It holds NO API key. The browser talks only to this app's own routes; the Palai credential is server-side
// in lib/palai.ts and lib/raw.ts, both guarded by `server-only`.
export function CodingChat() {
  const [sessionId, setSessionId] = useState<string | null>(null);
  const [bindingId, setBindingId] = useState("");
  const [input, setInput] = useState("");
  // The session id has to be readable by the transport's body callback WITHOUT re-creating the transport on
  // every turn, so it rides a ref alongside the state that renders it.
  const sessionRef = useRef<string | null>(null);
  const bindingRef = useRef("");
  bindingRef.current = bindingId;

  const { messages, sendMessage, status, error } = useChat({
    transport: new DefaultChatTransport({
      api: "/api/chat",
      // WHAT GOES UP IS THE HISTORY *AND* THE SESSION, and the route deliberately uses only the newest turn:
      // Palai already holds the conversation server-side, so replaying the history here would double every
      // prior turn into the context it re-sends anyway.
      body: () => ({
        sessionId: sessionRef.current,
        ...(bindingRef.current ? { bindingId: bindingRef.current } : {}),
      }),
    }),
    onData: (part) => {
      if (part.type === "data-session") {
        const id = (part.data as { sessionId?: string })?.sessionId;
        if (typeof id === "string") {
          sessionRef.current = id;
          setSessionId(id);
        }
      }
    },
  });

  const busy = status === "streaming" || status === "submitted";

  return (
    <main style={{ fontFamily: "ui-sans-serif, system-ui, sans-serif", maxWidth: 900, margin: "2rem auto", padding: "0 1rem" }}>
      <h1 style={{ marginBottom: "0.25rem" }}>Coding session</h1>
      <p style={{ color: "#666", marginTop: 0 }}>
        Vercel AI SDK <code>useChat</code> over Palai. Tell it what to do; the tools it runs and the approval it
        needs appear inline.
      </p>

      <p data-testid="chat-key-note" style={{ background: "#fff8e1", border: "1px solid #ffe0a3", padding: "0.6rem 0.8rem", borderRadius: 6 }}>
        <strong>No key in this page.</strong> The browser talks only to <code>/api/chat</code>; the Palai
        credential is server-side.
      </p>

      <div style={{ display: "flex", gap: "0.75rem", alignItems: "center", flexWrap: "wrap", margin: "1rem 0" }}>
        <input
          data-testid="chat-binding"
          value={bindingId}
          onChange={(e) => setBindingId(e.target.value)}
          placeholder="repository binding id (optional — repo_… to make it a coding session)"
          style={{ flex: "1 1 320px", padding: "0.5rem" }}
          aria-label="Repository binding id"
        />
        <span data-testid="chat-session" style={{ color: "#666", fontSize: "0.9rem" }}>
          {sessionId ? `session ${sessionId}` : "no session yet"}
        </span>
      </div>

      <ol data-testid="chat-messages" style={{ listStyle: "none", padding: 0, display: "grid", gap: "0.75rem" }}>
        {messages.map((m) => (
          <li key={m.id} data-testid={`chat-message-${m.role}`} style={{ border: "1px solid #ddd", borderRadius: 8, padding: "0.75rem" }}>
            <strong style={{ fontSize: "0.85rem", color: "#666" }}>{m.role}</strong>
            {m.parts.map((part, i) => (
              <Part key={i} part={part as Part} />
            ))}
          </li>
        ))}
      </ol>

      {error ? (
        <p data-testid="chat-error" style={{ color: "#b00" }}>
          {error.message}
        </p>
      ) : null}

      <form
        style={{ display: "flex", gap: "0.5rem", marginTop: "1rem" }}
        onSubmit={(e) => {
          e.preventDefault();
          if (input.trim() === "" || busy) return;
          sendMessage({ text: input });
          setInput("");
        }}
      >
        <input
          data-testid="chat-input"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          placeholder="e.g. read the README and tell me what this repo does"
          style={{ flex: 1, padding: "0.5rem", fontSize: "1rem" }}
          aria-label="Message"
        />
        <button data-testid="chat-send" type="submit" disabled={busy} style={{ padding: "0.5rem 1rem" }}>
          {busy ? "Working…" : "Send"}
        </button>
      </form>
    </main>
  );
}

type Part = { type: string; text?: string; data?: Record<string, unknown>; id?: string };

// Part renders ONE UI-message-stream part. The `data-*` arms are the Palai-specific half; `text` is the
// ecosystem's own and needs no translation at all, which is itself part of the answer.
function Part({ part }: { part: Part }) {
  if (part.type === "text") {
    return <p style={{ whiteSpace: "pre-wrap", margin: "0.5rem 0 0" }}>{part.text}</p>;
  }
  const d = (part.data ?? {}) as Record<string, unknown>;
  switch (part.type) {
    case "data-tool":
      return (
        <div data-testid="chat-tool" style={{ fontFamily: "ui-monospace, monospace", fontSize: "0.85rem", background: "#f6f6f6", padding: "0.5rem", borderRadius: 6, marginTop: "0.5rem" }}>
          {/* NO TOOL NAME, AND THE SCREEN SAYS SO RATHER THAN INVENTING ONE. Palai's tool_call frames carry
              only the call id and the replay class — measured on the journal — so an operator watching this
              chat can see THAT a tool ran and whether it was reversible, and cannot see WHICH. Rendering a
              placeholder in the name's position would read as a tool actually called that. */}
          <strong>tool call</strong> {String(d.id ?? "")} — {String(d.state ?? "")}
          {d.replayClass ? <span> ({String(d.replayClass)})</span> : null}
          {d.nameUnavailable ? (
            <div data-testid="chat-tool-name-gap" style={{ color: "#a60", marginTop: "0.25rem" }}>
              the tool&apos;s name is not carried on Palai&apos;s event stream
            </div>
          ) : null}
        </div>
      );
    case "data-approval":
      return <ApprovalButton data={d} />;
    case "data-publication":
      return (
        <p data-testid="chat-publication" style={{ marginTop: "0.5rem" }}>
          published <strong>{String(d.operation ?? "")}</strong>
          {d.receipt ? <code style={{ marginLeft: "0.4rem" }}>{JSON.stringify(d.receipt)}</code> : null}
        </p>
      );
    case "data-notice":
      return (
        <p data-testid="chat-notice" style={{ marginTop: "0.5rem", color: d.level === "error" ? "#b00" : "#a60" }}>
          {String(d.text ?? "")}
        </p>
      );
    case "data-run":
      return (
        <p data-testid="chat-run" style={{ marginTop: "0.5rem", color: "#666", fontSize: "0.85rem" }}>
          run {String(d.status ?? "")}
        </p>
      );
    default:
      return null;
  }
}

// ApprovalButton is the owner's sentence made true: the agent proposes a write, a button appears IN THE
// CHAT, a human presses it, and the run continues. It posts to the app's own relay, which forwards the
// one-shot request hash the run emitted — the binding that makes an approval authorize the exact call that
// was proposed rather than whatever it became.
function ApprovalButton({ data }: { data: Record<string, unknown> }) {
  const [state, setState] = useState<"open" | "sending" | "approved" | "denied" | "refused">("open");
  const [detail, setDetail] = useState("");

  async function decide(approve: boolean) {
    setState("sending");
    const res = await fetch("/api/chat/approve", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ approvalId: data.approvalId, requestHash: data.requestHash, approve }),
    });
    if (res.ok) {
      setState(approve ? "approved" : "denied");
      return;
    }
    // The three refusals have three different fixes and the relay passes the status through, so the chat
    // says which one it was rather than "failed".
    const body = (await res.json().catch(() => ({}))) as Record<string, unknown>;
    setState("refused");
    setDetail(String(body.detail ?? `HTTP ${res.status}`));
  }

  return (
    <div data-testid="chat-approval" style={{ marginTop: "0.5rem", border: "1px solid #f0c36d", background: "#fffbf0", borderRadius: 6, padding: "0.6rem" }}>
      <p style={{ margin: 0 }}>
        <strong>A human decision is needed</strong> — {String(data.operation ?? "a publication")}
      </p>
      {data.display ? <p style={{ margin: "0.25rem 0 0", color: "#555" }}>{String(data.display)}</p> : null}
      {state === "open" || state === "sending" ? (
        <div style={{ display: "flex", gap: "0.5rem", marginTop: "0.5rem" }}>
          <button data-testid="chat-approve" onClick={() => decide(true)} disabled={state === "sending"}>
            Approve
          </button>
          <button data-testid="chat-deny" onClick={() => decide(false)} disabled={state === "sending"}>
            Deny
          </button>
        </div>
      ) : (
        <p data-testid="chat-approval-outcome" style={{ margin: "0.5rem 0 0", color: state === "refused" ? "#b00" : "#060" }}>
          {state === "refused" ? `refused: ${detail}` : state}
        </p>
      )}
    </div>
  );
}
