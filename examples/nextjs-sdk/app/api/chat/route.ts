import { createUIMessageStream, createUIMessageStreamResponse } from "ai";

import { getPalaiClient } from "@/lib/palai";
import { rawBaseURL, rawHeaders } from "@/lib/raw";

export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// THE ADAPTER. This route is the whole answer to "can Palai be driven by the ecosystem's chat UI, or only
// by its own SDK", and the answer is in the mapping below rather than in this sentence.
//
// The browser runs `useChat` from @ai-sdk/react over `DefaultChatTransport`, which speaks the AI SDK's UI
// Message Stream protocol. Palai speaks its own SSE journal at GET /v1/sessions/{id}/events. Nothing
// connects them, so this handler translates: it opens a Palai session, posts the turn, reads Palai's frames
// and writes UI-message-stream parts.
//
// WHY NOT TextStreamChatTransport, which would have been three lines: the AI SDK's own docs say it carries
// no tool calls, no usage and no finish reason. A coding session whose whole point is that you WATCH it run
// a shell command and write a file would have shown a wall of text with the interesting half deleted.
//
// WHAT IS PROVEN AND WHAT IS NOT — read this before believing the demo:
//   - Palai's frames are journal events, not a token stream. `model_step.delta.v1` carries incremental text
//     when the engine emits it; a run that never deltas produces its text only at the terminal, so the chat
//     "types" in one go. That is Palai's shape and this adapter does not fake a stream it was not given.
//   - Palai has no notion of an assistant message id the client can reconcile against. This route mints one
//     per turn, which is fine for rendering and is NOT a persistence key.
//   - `useChat` sends the WHOLE message history on every turn. Palai does not want it: a session already
//     holds the conversation server-side, so only the newest user turn is forwarded. Sending the history
//     would double every prior turn into the context Palai already replays.
export async function POST(request: Request): Promise<Response> {
  let body: ChatRequest;
  try {
    body = (await request.json()) as ChatRequest;
  } catch {
    return problem(400, "invalid_request", "the request body must be JSON");
  }

  const text = latestUserText(body);
  if (text === null) {
    return problem(400, "invalid_request", "no user text in the newest message");
  }

  const client = getPalaiClient();

  // ONE SESSION PER CHAT, carried by the client in `body.sessionId`. A chat with no session opens one and
  // announces it as a data part, so the browser can send it back on the next turn — that is what makes this
  // a CONVERSATION rather than a series of unrelated single-shots, and it is the same session_id the
  // /compare page demonstrates.
  let sessionId = typeof body.sessionId === "string" && body.sessionId !== "" ? body.sessionId : null;

  const stream = createUIMessageStream({
    async execute({ writer }) {
      if (sessionId === null) {
        const session = await client.sessions.create();
        sessionId = session.id;
      }
      writer.write({ type: "data-session", data: { sessionId }, transient: true });

      const created = await client.responses.create({
        input: text,
        session_id: sessionId,
        ...(body.agentId ? { agent_id: body.agentId } : {}),
        ...(body.bindingId ? { repository: { binding_id: body.bindingId } } : {}),
      } as Parameters<typeof client.responses.create>[0]);

      writer.write({
        type: "data-run",
        data: { responseId: created.id, runId: created.run_id ?? null, status: created.status ?? "queued" },
      });

      const streamedText = await pumpPalaiFrames(writer, sessionId, created.id, request.signal);

      // THE TERMINAL ANSWER, AND THIS IS THE ONE THING THE FIRST VERSION OF THIS ROUTE GOT WRONG.
      //
      // It wrote text ONLY from `model_step.delta.v1` and shipped. Driven live, the chat rendered a session
      // id, a run id, "completed" — and NOT ONE WORD of the model's answer, because this run emitted no
      // delta frames at all. Palai's journal is an EVENT LOG, not a token stream: deltas appear when the
      // engine produces them and a single-step run simply finishes, with its text in the response's
      // `output`. The adapter has to read it, and a chat UI that renders everything except the reply is a
      // demo that proves the opposite of what it set out to.
      //
      // It is guarded on `streamedText` so a run that DID delta is not made to say everything twice.
      if (!streamedText) {
        // AND IT HAS TO WAIT FOR THE PROJECTION, which is the second thing this block got wrong.
        //
        // `run.completed.v1` is journalled BEFORE the response's `output` is readable. Retrieving the
        // instant the terminal frame lands returned an EMPTY output and the chat rendered a tool call, a
        // "completed", and no answer — which is what the first fix looked like when a tool was involved.
        // The plain single-step turn happened to win the race, so it looked fixed; the coding turn did not.
        // A terminal frame is not a promise that the projection is queryable, so this re-reads briefly.
        let final = await client.responses.retrieve(created.id);
        for (let i = 0; i < 20 && (final.output ?? []).length === 0; i += 1) {
          await new Promise((r) => setTimeout(r, 500));
          final = await client.responses.retrieve(created.id);
        }
        const text = (final.output ?? [])
          .map((item) => (item as { content?: unknown }).content)
          .filter((c): c is string => typeof c === "string")
          .join("\n");
        if (text !== "") {
          const id = `palai-final-${created.id}`;
          writer.write({ type: "text-start", id });
          writer.write({ type: "text-delta", id, delta: text });
          writer.write({ type: "text-end", id });
        }
        if (final.usage) {
          writer.write({ type: "data-usage", data: final.usage as Record<string, unknown>, transient: true });
        }
      }
    },
    // A THROWN ERROR MUST REACH THE CHAT, not vanish into a closed stream. The AI SDK swallows the exception
    // and calls this for the sentence the client renders.
    onError: (error) => (error instanceof Error ? error.message : "the Palai stream failed"),
  });

  return createUIMessageStreamResponse({ stream });
}

// pumpPalaiFrames reads Palai's SSE journal for this session and writes the UI parts. It is the mapping
// table, and the comments on each arm are the finding rather than documentation of the obvious.
async function pumpPalaiFrames(
  writer: StreamWriter,
  sessionId: string,
  responseId: string,
  signal: AbortSignal,
): Promise<boolean> {
  const res = await fetch(`${rawBaseURL()}/v1/sessions/${encodeURIComponent(sessionId)}/events`, {
    headers: { ...rawHeaders(), Accept: "text/event-stream" },
    signal,
  });
  if (!res.ok || res.body === null) {
    throw new Error(`Palai event stream answered HTTP ${res.status}`);
  }

  const textId = `palai-${responseId}`;
  let textOpen = false;
  let streamed = false;
  const openText = () => {
    if (!textOpen) {
      writer.write({ type: "text-start", id: textId });
      textOpen = true;
      streamed = true;
    }
  };

  const reader = res.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });

      // SSE frames are separated by a BLANK LINE, and a chunk boundary can fall anywhere — including inside
      // a JSON payload. Splitting on "\n" and parsing each line is the bug this loop exists not to have.
      let sep: number;
      while ((sep = buffer.indexOf("\n\n")) !== -1) {
        const frame = buffer.slice(0, sep);
        buffer = buffer.slice(sep + 2);
        const evt = parseFrame(frame);
        if (evt === null) continue;

        const terminal = writeFrame(writer, evt, openText, textId, responseId);
        if (terminal) {
          if (textOpen) {
            writer.write({ type: "text-end", id: textId });
            textOpen = false;
          }
          return streamed;
        }
      }
    }
  } finally {
    if (textOpen) {
      try {
        writer.write({ type: "text-end", id: textId });
      } catch {
        // The stream is already closed (the browser navigated away); there is nothing to end.
      }
    }
    try {
      await reader.cancel();
    } catch {
      // Already torn down.
    }
  }
  return streamed;
}

// writeFrame maps ONE Palai journal frame onto UI-message-stream parts. Returns true when the run reached a
// terminal state and the pump should stop.
//
// THE FRAMES THAT DO NOT MAP ARE THE FINDING, and they are listed here rather than silently dropped:
//   - attempt.recovering.v1 / recovery.proof.v1 — Palai recovered a crashed attempt and replayed from a
//     checkpoint. The AI SDK protocol has no notion of "that last bit did not happen, here it is again";
//     its parts are append-only. Rendered as a data-notice so the operator is not lied to by silence.
//   - tool_call.uncertain.v1 / tool_call.manual_resolution.v1 — a tool whose result was never committed.
//     There IS no AI SDK part for "this tool call is in limbo and a human must resolve it": a tool part is
//     input-available or output-available. Rendered as a data-notice, and it is the visible face of the
//     wedge (a tool that errors leaves the run here forever).
//   - checkpoint.offer.v1, run.provisioning.v1, command.accepted.v1 — pure infrastructure with no UI
//     meaning. Dropped deliberately.
function writeFrame(
  writer: StreamWriter,
  evt: { type: string; data: Record<string, unknown> },
  openText: () => void,
  textId: string,
  responseId: string,
): boolean {
  const d = evt.data;
  switch (evt.type) {
    case "model_step.delta.v1": {
      if (typeof d.text === "string" && d.text !== "") {
        openText();
        writer.write({ type: "text-delta", id: textId, delta: d.text });
      }
      return false;
    }

    // A TOOL CALL BECOMES A TOOL PART — the half TextStreamChatTransport would have thrown away and the
    // reason this route speaks the richer protocol.
    //
    // AND IT IS THE SHARPEST THING THAT DOES NOT MAP. The AI SDK's tool part wants a NAME and an
    // input/output pair. Palai's tool frames carry NEITHER. Measured on the journal, not assumed:
    //   tool_call.executing.v1 -> {"run_id","replay_class","tool_call_id"}
    //   tool_call.completed.v1 -> {"run_id","tool_call_id"}
    // That is the whole payload. There is no `name`, no `arguments`, no `result` anywhere in the stream —
    // they live on the tool_calls ledger, which the events API does not join.
    //
    // My first version wrote `name: d.name ?? null` and the chat rendered a tool part labelled "tool",
    // which READ like a tool whose name happened to be "tool". Printing a field the frame never carried is
    // how a demo tells a confident lie, so this renders what IS carried — the call id and the replay class,
    // which at least tells an operator whether the thing that just ran was reversible.
    //
    // CLOSING IT is a control-plane change (put the name on the frame, or give the events API a join), not
    // an adapter change, and inventing a second lookup here would hide the gap rather than report it.
    case "tool_call.executing.v1":
      writer.write({
        type: "data-tool",
        id: String(d.tool_call_id ?? "tool"),
        data: {
          id: d.tool_call_id ?? null,
          replayClass: d.replay_class ?? null,
          state: "running",
          nameUnavailable: true,
        },
      });
      return false;
    case "tool_call.completed.v1":
      writer.write({
        type: "data-tool",
        id: String(d.tool_call_id ?? "tool"),
        data: { id: d.tool_call_id ?? null, state: "done", nameUnavailable: true },
      });
      return false;

    // THE APPROVAL, AND THIS IS THE PART THE OWNER ASKED FOR. It is a custom data part precisely because the
    // AI SDK has no "a human must authorize this before the run continues" concept — the closest thing
    // (tool approval) is about a tool the CLIENT executes, and this is a decision the CONTROL PLANE is
    // parked on. The browser renders it as a button and answers it over /api/chat/approve.
    case "approval.requested.v1":
      writer.write({
        type: "data-approval",
        id: String(d.approval_id ?? d.publication_id ?? "approval"),
        data: {
          approvalId: d.approval_id ?? null,
          requestHash: d.request_hash ?? null,
          operation: d.operation ?? null,
          display: d.display ?? null,
        },
      });
      return false;

    case "publication.published.v1":
      writer.write({
        type: "data-publication",
        id: String(d.publication_id ?? "publication"),
        data: { publicationId: d.publication_id ?? null, operation: d.operation ?? null, receipt: d.receipt ?? null },
      });
      return false;

    case "usage.updated.v1":
      writer.write({ type: "data-usage", data: pickUsage(d), transient: true });
      return false;

    case "attempt.recovering.v1":
    case "recovery.proof.v1":
      writer.write({
        type: "data-notice",
        data: { level: "warn", text: "Palai recovered a crashed attempt and replayed from a checkpoint." },
      });
      return false;

    case "tool_call.uncertain.v1":
    case "tool_call.manual_resolution.v1":
      writer.write({
        type: "data-notice",
        data: {
          level: "error",
          text:
            "A tool call could not be resolved and this run is now parked waiting for a human. It will not " +
            "continue on its own. (This is what a tool returning an error looks like from the chat.)",
        },
      });
      return false;

    case "run.failed.v1":
    case "run.cancelled.v1":
    case "run.completed.v1":
      writer.write({ type: "data-run", data: { responseId, status: evt.type.split(".")[1] } });
      return true;

    default:
      return false;
  }
}

function parseFrame(frame: string): { type: string; data: Record<string, unknown> } | null {
  let type = "";
  const dataLines: string[] = [];
  for (const line of frame.split("\n")) {
    if (line.startsWith("event:")) type = line.slice(6).trim();
    else if (line.startsWith("data:")) dataLines.push(line.slice(5).trim());
  }
  if (type === "" || dataLines.length === 0) return null;
  try {
    const envelope = JSON.parse(dataLines.join("\n")) as Record<string, unknown>;
    // Palai's SSE payload is a CloudEvent envelope; the frame's own fields are under `data`.
    const inner = (envelope.data ?? {}) as Record<string, unknown>;
    return { type, data: inner };
  } catch {
    return null;
  }
}

function pickUsage(d: Record<string, unknown>): Record<string, unknown> {
  return {
    input_tokens: typeof d.input_tokens === "number" ? d.input_tokens : null,
    output_tokens: typeof d.output_tokens === "number" ? d.output_tokens : null,
    total_tokens: typeof d.total_tokens === "number" ? d.total_tokens : null,
  };
}

function latestUserText(body: ChatRequest): string | null {
  const messages = Array.isArray(body.messages) ? body.messages : [];
  for (let i = messages.length - 1; i >= 0; i -= 1) {
    const m = messages[i];
    if (m?.role !== "user") continue;
    const parts = Array.isArray(m.parts) ? m.parts : [];
    const text = parts
      .filter((p) => p?.type === "text" && typeof p.text === "string")
      .map((p) => p.text as string)
      .join("");
    return text === "" ? null : text;
  }
  return null;
}

interface ChatRequest {
  messages?: { role?: string; parts?: { type?: string; text?: unknown }[] }[];
  sessionId?: unknown;
  agentId?: string;
  bindingId?: string;
}

type StreamWriter = { write: (part: Record<string, unknown>) => void };

function problem(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
