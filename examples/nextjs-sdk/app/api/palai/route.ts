import { PalaiAPIError, PalaiError, type Event, type Response as PalaiResponse } from "@palai/sdk";

import { getPalaiClient } from "@/lib/palai";

// The SDK's server path uses node:crypto, so this runs on the Node runtime; force-dynamic
// keeps it out of the static build (the credential is never present at build time).
export const runtime = "nodejs";
export const dynamic = "force-dynamic";

// POST re-projects the SDK's canonical Event stream to the browser as newline-delimited JSON
// (application/x-ndjson). It forwards ONLY curated canonical fields — never the raw provider
// payload and never the credential. The browser talks only to this handler; the API key
// stays server-side (see lib/palai.ts). A browser disconnect aborts request.signal, which
// closes the upstream transport but does NOT cancel the run: the SDK stops iterating on abort
// and never calls cancel (LP6: SSE disconnect ≠ cancel; server-side cancel is Task 14b).
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

        for await (const event of palaiStream) {
          enqueue(projectEvent(event));
        }

        // The stream ended. If the browser is still connected (not an abort), project the
        // canonical terminal Response — status, the server-selected model, output, usage.
        if (!request.signal.aborted && palaiStream.responseID) {
          const final = await client.responses.retrieve(palaiStream.responseID);
          enqueue(projectFinal(final));
        }
      } catch (err) {
        // A browser disconnect surfaces as an abort here; there is no client left to tell.
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

// projectEvent maps a canonical Event to a curated browser projection. Data-plane events
// (text delta, tool, usage) carry their canonical payload fields; every other event projects
// only its type + sequence, feeding the ordered timeline without leaking any raw payload.
export function projectEvent(event: Event): Record<string, unknown> {
  const data = (event.data ?? {}) as Record<string, unknown>;
  const head = { type: event.type, sequence: event.sequence };
  switch (event.type) {
    case "model_step.delta.v1":
      return typeof data.text === "string" ? { ...head, text: data.text } : head;
    case "tool_call.proposed.v1":
    case "tool_call.ready.v1":
      return { ...head, tool: { id: data.tool_call_id, name: data.name, arguments: data.arguments ?? null } };
    case "tool_call.completed.v1":
      return { ...head, tool: { id: data.tool_call_id, name: data.name, result: data.result ?? null } };
    case "usage.updated.v1":
      return { ...head, usage: pickUsage(data) };
    // SUBAGENTS. A delegated run is a CHILD: the parent journals that it asked for one, and later that
    // the child finished or was denied admission. The child's OWN model steps never enter this stream —
    // it has its own session — so what a reader can see here is the delegation itself, and that is worth
    // showing: an agent that spawned four children and is waiting on them looks identical to an agent
    // that is stuck, unless the delegation is on the screen.
    case "child.requested.v1":
      return { ...head, child: { id: data.child_run_id ?? null, requestId: data.child_request_id ?? null, state: "requested" } };
    case "child.completed.v1":
      return { ...head, child: { id: data.child_run_id ?? null, requestId: data.child_request_id ?? null, state: "completed", status: data.status ?? null } };
    // THE FIELD NAMES ARE MEASURED, not guessed: child_dispatch.go journals child_request_id,
    // child_run_id, status and output. An earlier draft here read `run_id`, which is the PARENT's field
    // name and is simply absent from these events — it would have rendered every subagent as "unknown".
    //
    // A DENIAL IS NOT A FAILURE OF THE CHILD, it is the admission layer refusing to start one — a budget,
    // a depth limit, a policy. Rendering it as an error would send a reader looking at the wrong thing.
    case "child.denied.v1":
      return { ...head, child: { id: data.child_run_id ?? null, requestId: data.child_request_id ?? null, state: "denied", reason: data.reason ?? null } };
    default:
      return head;
  }
}

// projectFinal carries the canonical terminal Response fields the UI renders — never the raw
// provider output. On a failed terminal the Response.error is a problem document; surface its
// stable code + request id.
function projectFinal(final: PalaiResponse): Record<string, unknown> {
  const projection: Record<string, unknown> = {
    type: "response.final",
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

// projectError renders a stable, code-carrying error — never raw provider text.
function projectError(err: unknown): Record<string, unknown> {
  if (err instanceof PalaiAPIError) {
    return {
      type: "error",
      code: err.code,
      requestId: err.requestId ?? null,
      status: err.status,
      detail: err.problem.detail ?? err.problem.title ?? err.code,
    };
  }
  if (err instanceof PalaiError) {
    return { type: "error", code: "connection_error", requestId: null, detail: err.message };
  }
  return { type: "error", code: "internal_error", requestId: null, detail: "the response stream failed" };
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
