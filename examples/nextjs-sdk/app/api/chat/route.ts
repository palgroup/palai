import { createUIMessageStream, createUIMessageStreamResponse } from "ai";
import type { UIMessageStreamWriter } from "ai";

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
          writer.write({ type: "data-usage", data: final.usage as unknown as Record<string, unknown>, transient: true });
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
  // Frames that need a server-side join (the approval) start a fetch rather than blocking the pump:
  // a stalled join must not stop the run's own frames from reaching the chat. They are awaited
  // before the pump returns so the stream is never closed with a part still in flight.
  const pending: Promise<void>[] = [];

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

        const terminal = writeFrame(writer, evt, openText, textId, responseId, pending);
        if (terminal) {
          if (textOpen) {
            writer.write({ type: "text-end", id: textId });
            textOpen = false;
          }
          await Promise.allSettled(pending);
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
  pending: Promise<void>[],
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
    //
    // THE FRAME IS A NOTIFICATION, NOT A PAYLOAD, AND THE OLD CODE HERE COULD NOT HAVE WORKED.
    //
    // MEASURED 2026-08-02 (coordinator/publication.go:269-272 — the emitter): a PUBLICATION approval
    // frame carries exactly
    //     {publication_id, operation, branch, request_hash, display}
    // It carries NO approval_id. The version of this arm on main wrote `approvalId: d.approval_id ?? null`,
    // so the button sent approvalId=null and /api/chat/approve answered its own 400 —
    // "approvalId and requestHash are both required" — before the control plane was ever asked.
    // The approval path WAS fixed control-plane side on 2026-08-01 and is provable with curl; the
    // CHAT SURFACE was never driven, which is exactly the gap CLAUDE.md names: proving a mechanism
    // is not proving the surface a human uses.
    //
    // It also carries no remote, no base, no head_sha and no credential identity — so a screen that
    // wants to say WHERE the write goes and AS WHOM cannot get it from the stream at all. The
    // coordinator says so on purpose (approvals.go:162): "the genesis event carries the BINDING,
    // never a rendered screen."
    //
    // So the join happens HERE, server-side, where the credential already is: GET /v1/approvals and
    // match on publication_id. The browser gets one part with everything the confirmation needs.
    case "approval.requested.v1":
      pending.push(
        joinApproval(writer, {
          publicationId: str(d.publication_id),
          approvalId: str(d.approval_id),
          requestHash: str(d.request_hash),
          operation: str(d.operation),
          branch: str(d.branch),
          display: str(d.display),
        }),
      );
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

// joinApproval turns a publication approval NOTIFICATION into the row a human can decide on.
//
// The join key is `publication_id`, not `id`: GET /v1/approvals returns a row whose own `id` is the
// approval id (apr_…) and whose `publication_id` field is what the frame carried
// (api/approvals.go:79). Matching on `id` would never hit.
//
// WHAT THE ROW ADDS OVER THE FRAME, all measured on api/approvals.go:52-95 —
//   remote, branch, base, head_sha       WHERE the write lands
//   credential_ref, credential           AS WHOM it is made
// `credential` is one of two fixed sentences the control plane chooses with the SAME condition the
// publisher branches on (store/approvals.go:269): an empty credential_ref means the deployment's
// GitHub App, NOT an unknown identity. The screen must not render an empty ref as a blank, because
// blank reads as "nobody checked" when it actually means "a different, named identity".
//
// IF THE JOIN FAILS the part is still written, with joined:false — the operator gets a decidable
// button carrying the hash the frame did give, and the screen says the destination could not be
// read. Dropping the part on a failed join would hide an approval that is genuinely pending and
// leave the run parked with nothing on screen.
async function joinApproval(
  writer: StreamWriter,
  frame: { publicationId: string; approvalId: string; requestHash: string; operation: string; branch: string; display: string },
): Promise<void> {
  const base = {
    approvalId: frame.approvalId !== "" ? frame.approvalId : null,
    requestHash: frame.requestHash !== "" ? frame.requestHash : null,
    operation: frame.operation !== "" ? frame.operation : null,
    branch: frame.branch !== "" ? frame.branch : null,
    display: frame.display !== "" ? frame.display : null,
    publicationId: frame.publicationId !== "" ? frame.publicationId : null,
  };
  const partId = frame.publicationId !== "" ? frame.publicationId : frame.approvalId !== "" ? frame.approvalId : "approval";

  try {
    const res = await fetch(`${rawBaseURL()}/v1/approvals`, { headers: rawHeaders(), cache: "no-store" });
    if (!res.ok) throw new Error(`GET /v1/approvals answered HTTP ${res.status}`);
    const page = (await res.json()) as { data?: Record<string, unknown>[] };
    const rows = Array.isArray(page.data) ? page.data : [];
    const row =
      rows.find((r) => frame.publicationId !== "" && str(r.publication_id) === frame.publicationId) ??
      rows.find((r) => frame.approvalId !== "" && str(r.id) === frame.approvalId) ??
      null;

    if (row === null) {
      writer.write({
        type: "data-approval",
        id: partId,
        data: { ...base, joined: false, joinNote: "this publication is not on GET /v1/approvals — it may already have been decided" },
      });
      return;
    }

    writer.write({
      type: "data-approval",
      id: partId,
      data: {
        ...base,
        joined: true,
        // The row's own id is the one POST /v1/approvals/{id}/approve wants. The frame never had it.
        approvalId: str(row.id) !== "" ? str(row.id) : base.approvalId,
        requestHash: str(row.request_hash) !== "" ? str(row.request_hash) : base.requestHash,
        operation: str(row.operation) !== "" ? str(row.operation) : base.operation,
        remote: str(row.remote) || null,
        branch: str(row.branch) || base.branch,
        baseBranch: str(row.base) || null,
        headSha: str(row.head_sha) || null,
        credentialRef: str(row.credential_ref) || null,
        credential: str(row.credential) || null,
        expiresAt: str(row.expires_at) || null,
        operatorLabel: str(row.operator_label) || null,
      },
    });
  } catch (error) {
    writer.write({
      type: "data-approval",
      id: partId,
      data: { ...base, joined: false, joinNote: error instanceof Error ? error.message : "the approval row could not be read" },
    });
  }
}

function str(value: unknown): string {
  return typeof value === "string" ? value.trim() : "";
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

// StreamWriter is the AI SDK's OWN writer, narrowed to the one method used. It was a hand-rolled
// `{ write: (part: Record<string, unknown>) => void }`, which is wider than the real chunk union and
// therefore accepted a part the protocol has no place for. Nothing caught that, because this
// example had no working type check at all until tsconfig.json was fixed in this commit.
type StreamWriter = Pick<UIMessageStreamWriter, "write">;

function problem(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
