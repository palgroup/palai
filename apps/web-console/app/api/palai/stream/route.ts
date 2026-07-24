import { PalaiAPIError, PalaiError, type Event, type Response as PalaiResponse } from "@palai/sdk";

import { getPalaiClient } from "@/lib/palai";
import { laneFor } from "@/lib/timeline";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic keeps the
// credential out of the static build.
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// POST starts a run and re-projects the SDK's canonical Event stream to the browser as newline-delimited
// JSON. It forwards ONLY curated canonical fields, sorted into the §47.2 lanes by laneFor — never the raw
// provider payload and never the credential. The browser talks only to this handler; the API key stays
// server-side (lib/palai.ts). The stream stays open across a waiting_for_approval pause: the browser
// resolves the approval through the /v1/* command relay, and the upstream then continues to terminal.
//
// The first frame is a `meta` carrying the session + response id so the browser can address the approve
// command and list the run's artifacts — both through the same /v1/* relay, never a backchannel.
export async function POST(request: Request): Promise<Response> {
  let prompt: string;
  try {
    const body = (await request.json()) as { prompt?: unknown };
    if (typeof body.prompt !== "string" || body.prompt.trim() === "") {
      return problemResponse(400, "invalid_request", "a non-empty 'prompt' string is required");
    }
    prompt = body.prompt;
  } catch {
    return problemResponse(400, "invalid_request", "request body must be JSON with a 'prompt' string");
  }

  const client = getPalaiClient();
  const encoder = new TextEncoder();

  const stream = new ReadableStream<Uint8Array>({
    async start(controller) {
      const enqueue = (obj: Record<string, unknown>) => {
        controller.enqueue(encoder.encode(`${JSON.stringify(obj)}\n`));
      };
      try {
        const palaiStream = client.responses.stream({ input: prompt }, { signal: request.signal });
        enqueue({ type: "status", status: "streaming" });

        let metaSent = false;
        for await (const event of palaiStream) {
          // Emit the session + response id as soon as the create has resolved (before the first
          // projected event), so the browser can address the approve/deny command and the artifact
          // list mid-stream — both through the /v1/* relay, never a backchannel.
          if (!metaSent && palaiStream.sessionID) {
            enqueue({ type: "meta", sessionId: palaiStream.sessionID, responseId: palaiStream.responseID ?? null });
            metaSent = true;
          }
          enqueue(projectEvent(event));
        }

        // The stream reached its terminal. If the browser is still connected, project the canonical
        // terminal Response — status, the server-selected model, output, usage.
        if (!request.signal.aborted && palaiStream.responseID) {
          const final = await client.responses.retrieve(palaiStream.responseID);
          enqueue(projectFinal(final));
        }
      } catch (err) {
        if (!request.signal.aborted) {
          enqueue(projectError(err));
        }
      } finally {
        try {
          controller.close();
        } catch {
          // Already closed because the browser disconnected — nothing to do.
        }
      }
    },
  });

  return new Response(stream, {
    headers: {
      "Content-Type": "application/x-ndjson; charset=utf-8",
      "Cache-Control": "no-cache, no-transform",
      "X-Content-Type-Options": "nosniff",
    },
  });
}

// projectEvent maps a canonical Event to a curated, lane-tagged browser projection. Each lane carries
// only the fields the timeline renders. The approval lane is the SECURITY-CRITICAL one (UI-002): it
// forwards the AUTHORITATIVE detail (action / args / diff / destination / risk / expiry) as distinct
// fields AND the model's summary SEPARATELY — the UI renders the authoritative detail and never lets the
// summary substitute for it.
function projectEvent(event: Event): Record<string, unknown> {
  const data = (event.data ?? {}) as Record<string, unknown>;
  const lane = laneFor(event.type);
  const head = { lane, type: event.type, sequence: event.sequence, time: event.time };
  switch (lane) {
    case "model_step":
      return typeof data.text === "string" ? { ...head, text: data.text } : head;
    case "tool":
      return {
        ...head,
        tool: {
          id: data.tool_call_id ?? data.child_id ?? null,
          name: data.name ?? null,
          arguments: data.arguments ?? null,
          result: data.result ?? null,
          progress: data.progress ?? null,
        },
      };
    case "approval":
      return {
        ...head,
        approval: {
          id: data.approval_id ?? data.publication_id ?? null,
          resolution: approvalResolution(event.type),
          // The authoritative, model-independent detail (§47.2 / UI-002).
          action: data.action ?? null,
          args: data.args ?? null,
          diff: data.diff ?? null,
          destination: data.destination ?? null,
          risk: data.risk ?? null,
          expires_at: data.expires_at ?? null,
          // The model's own words — kept SEPARATE so the UI never substitutes it for the detail above.
          model_summary: data.model_summary ?? null,
        },
      };
    case "recovery":
      return { ...head, recovery: { attempt: data.attempt ?? null, detail: data.detail ?? null, checkpoint_id: data.checkpoint_id ?? null } };
    case "usage":
      return { ...head, usage: pickUsage(data) };
    default:
      return head;
  }
}

function approvalResolution(type: string): string | null {
  switch (type) {
    case "approval.approved.v1":
      return "approved";
    case "approval.denied.v1":
      return "denied";
    case "approval.expired.v1":
      return "expired";
    default:
      return null; // approval.requested.v1 — still pending
  }
}

function projectFinal(final: PalaiResponse): Record<string, unknown> {
  const projection: Record<string, unknown> = {
    type: "final",
    lane: "terminal",
    status: final.status,
    model: final.model,
    output: final.output,
    usage: pickUsage((final.usage ?? {}) as unknown as Record<string, unknown>),
  };
  if (final.error) {
    projection.error = { code: final.error.code, requestId: final.error.request_id, detail: final.error.detail ?? final.error.title };
  }
  return projection;
}

function projectError(err: unknown): Record<string, unknown> {
  if (err instanceof PalaiAPIError) {
    return { type: "error", lane: "terminal", code: err.code, requestId: err.requestId ?? null, status: err.status, detail: err.problem.detail ?? err.problem.title ?? err.code };
  }
  if (err instanceof PalaiError) {
    return { type: "error", lane: "terminal", code: "connection_error", requestId: null, detail: err.message };
  }
  return { type: "error", lane: "terminal", code: "internal_error", requestId: null, detail: "the response stream failed" };
}

function pickUsage(data: Record<string, unknown>): Record<string, unknown> {
  return {
    input_tokens: numberOr(data.input_tokens),
    output_tokens: numberOr(data.output_tokens),
    total_tokens: numberOr(data.total_tokens),
    tool_calls: numberOr(data.tool_calls),
  };
}

function numberOr(value: unknown): number | null {
  return typeof value === "number" ? value : null;
}

function problemResponse(status: number, code: string, detail: string): Response {
  return new Response(JSON.stringify({ type: `https://docs.palai.dev/problems/${code}`, title: code, status, code, detail }), {
    status,
    headers: { "Content-Type": "application/problem+json; charset=utf-8", "Cache-Control": "no-store" },
  });
}
